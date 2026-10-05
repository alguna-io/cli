package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// usageError is a wrong command line: an unknown command or flag, a missing
// argument, a missing required field. It exits with 1 and points to the help of
// the command that failed.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// apiError is a non-2xx response.
type apiError struct {
	status    int
	requestID string
	body      []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.status, http.StatusText(e.status))
}

// maxPlainTextMessage is the longest plain-text response body that a terminal
// error shows. A longer body, or an HTML error page from a proxy, would flood
// the terminal; the JSON form of the error keeps every body whole.
const maxPlainTextMessage = 200

// message is the human text of the response body: the detail, else the title of
// an ErrorResponse, else the message of a GatewayErrorResponse, else a short
// one-line plain-text body, else the HTTP status text.
func (e *apiError) message() string {
	var body struct {
		Detail  string `json:"detail"`
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(e.body, &body); err == nil {
		for _, text := range []string{body.Detail, body.Title, body.Message} {
			if text = strings.TrimSpace(text); text != "" {
				return text
			}
		}
	} else if text := strings.TrimSpace(string(e.body)); text != "" && len(text) <= maxPlainTextMessage && !strings.ContainsAny(text, "<\n") {
		return text
	}
	return http.StatusText(e.status)
}

// networkError is a request that got no response.
type networkError struct{ err error }

func (e *networkError) Error() string { return e.err.Error() }
func (e *networkError) Unwrap() error { return e.err }

type errorOutput struct {
	Error     string          `json:"error"`
	Status    int             `json:"status,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
	Hint      string          `json:"hint,omitempty"`
}

// writeError prints err to w and returns the exit code. The human form is one
// line, plus a hint line; the JSON form is one object. command is the path of
// the command that failed, such as "alguna customers list".
func writeError(w io.Writer, err error, command string, human bool) int {
	out := errorOutput{Error: err.Error()}
	line := err.Error()
	code := 1
	var apiErr *apiError
	var netErr *networkError
	var usage *usageError
	switch {
	case errors.As(err, &apiErr):
		out.Status = apiErr.status
		out.RequestID = apiErr.requestID
		out.Response = asJSON(apiErr.body)
		line = fmt.Sprintf("%s (HTTP %d", apiErr.message(), apiErr.status)
		if apiErr.requestID != "" {
			line += ", request " + apiErr.requestID
		}
		line += ")"
		code = exitServerError
		if apiErr.status >= 400 && apiErr.status < 500 {
			code = exitClientError
		}
		if apiErr.status == http.StatusUnauthorized {
			out.Hint = "Check your API key, or run 'alguna login'."
		}
	case errors.As(err, &netErr):
		code = exitServerError
		if errors.Is(err, context.DeadlineExceeded) {
			out.Hint = "Use --timeout to wait longer for each attempt (default " + defaultTimeout.String() + ")."
		}
	case errors.As(err, &usage):
		out.Hint = fmt.Sprintf("Run '%s --help' for usage.", command)
	}
	if !human {
		_ = encodeJSON(w, out, "")
		return code
	}
	fmt.Fprintln(w, "Error: "+line)
	if out.Hint != "" {
		fmt.Fprintln(w, out.Hint)
	}
	return code
}

// asJSON returns body unchanged when it is JSON, and as a JSON string otherwise.
func asJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if json.Valid(body) {
		return body
	}
	var quoted bytes.Buffer
	_ = encodeJSON(&quoted, string(body), "")
	return bytes.TrimSpace(quoted.Bytes())
}
