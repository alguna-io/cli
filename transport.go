package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The retry rules are the generated SDKs' rules. Every request the CLI sends is a
// GET, carries an Idempotency-Key, or is an operation that the API identifies by
// its body (newRequest), so a retry cannot apply a write twice and any request may
// be retried.
const (
	defaultMaxRetries = 2
	defaultTimeout    = time.Minute
	firstRetryWait    = time.Second
	// maxRetryWait caps the backoff and the wait a Retry-After header asks for.
	maxRetryWait = time.Minute
)

const idempotencyKeyHeader = "Idempotency-Key"

// request is one HTTP request, before it is sent.
type request struct {
	method string
	url    *url.URL
	header http.Header
	// body is held whole in memory, so every attempt sends it again.
	body []byte
}

type transport struct {
	client *http.Client
	sleep  func(time.Duration)
	// timeout limits each attempt, so a retry gets the full time again.
	timeout    time.Duration
	maxRetries uint
}

// send returns the body of a 2xx response, or an *apiError or *networkError.
func (t transport) send(ctx context.Context, req *request) ([]byte, error) {
	for retries := uint(0); ; retries++ {
		status, header, body, err := t.do(ctx, req)
		var failure error
		switch {
		case err != nil:
			failure = &networkError{err: err}
			if !retryableNetworkError(ctx, err) {
				return nil, failure
			}
		case status >= 200 && status < 300:
			return body, nil
		default:
			failure = &apiError{status: status, requestID: header.Get("X-Request-Id"), body: body}
			if !retryableStatus(status, header) {
				return nil, failure
			}
		}
		if retries == t.maxRetries {
			return nil, failure
		}
		t.sleep(retryWait(retries+1, header, time.Now()))
	}
}

// retryableNetworkError reports whether a retry can fix err: a refused or reset
// connection, a connection closed before the response, or a timeout. A cancelled
// or expired ctx is the caller's decision, so it is never retried.
func retryableNetworkError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var opErr *net.OpError
	var netErr net.Error
	return errors.As(err, &opErr) || (errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryableStatus reports whether a retry can fix the response. A 409 is a real
// conflict unless it carries Retry-After, which the API sends only while a request
// with the same Idempotency-Key is still running.
func retryableStatus(status int, header http.Header) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusConflict:
		return header.Get("Retry-After") != ""
	default:
		return false
	}
}

// retryWait is the wait before the given retry (1 for the first): the response's
// Retry-After when it is valid, else exponential backoff from 1 second with ±10%
// jitter, so clients that failed together do not retry together. header is nil
// after a network error.
func retryWait(retry uint, header http.Header, now time.Time) time.Duration {
	if wait, ok := retryAfter(header.Get("Retry-After"), now); ok {
		return wait
	}
	// The shift stops at 64 s, above the cap, so it cannot overflow.
	backoff := firstRetryWait << min(retry-1, 6)
	return min(backoff-backoff/10+rand.N(backoff/5), maxRetryWait)
}

// retryAfter parses a Retry-After value, a number of seconds or an HTTP date,
// and caps it at maxRetryWait. A date in the past means no wait.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(min(seconds, int64(maxRetryWait/time.Second))) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), maxRetryWait), true
	}
	return 0, false
}

func (t transport) do(ctx context.Context, req *request) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, req.url.String(), body)
	if err != nil {
		return 0, nil, nil, err
	}
	httpReq.Header = req.header.Clone()
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}
