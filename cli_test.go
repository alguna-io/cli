package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

const envKey = "key_env.secret_env"

// testManifest has one method for each runtime behaviour, so the tests do not
// depend on the API spec.
var testManifest = Manifest{
	BaseURL:    "https://api.alguna.io",
	APIVersion: "2026-04-01",
	Commands: []Command{{
		Name:        "things",
		Description: "Things",
		Methods: []Method{
			{
				Name: "list", Summary: "List things", HTTPMethod: http.MethodGet, APIPath: "/things",
				Options: []Option{
					{Name: "status", Field: "status", In: inQuery, Type: typeString, Enum: []string{"active", "archived"}},
					{Name: "tag-ids", Field: "tag_ids", In: inQuery, Type: typeStringList},
					{Name: "include-archived", Field: "include_archived", In: inQuery, Type: typeBoolean},
					{Name: "limit", Field: "limit", In: inQuery, Type: typeInteger},
					{Name: "offset", Field: "offset", In: inQuery, Type: typeInteger},
				},
				// A small spec maximum, so the tests page through a few items.
				Pagination: &Pagination{Offset: "offset", Limit: "limit", Results: "data", MaxLimit: 2},
				Epilogue:   "Example:\n  alguna things list --all",
			},
			{
				Name: "lookup", Summary: "Look up a thing", HTTPMethod: http.MethodGet, APIPath: "/things/lookup",
				Options: []Option{{Name: "alias", Field: "alias", In: inQuery, Type: typeString, Required: true}},
			},
			{
				Name: "get", Summary: "Get a thing", HTTPMethod: http.MethodGet, APIPath: "/things/{id}",
				Arguments: []Argument{{Name: "id", Description: "The thing ID"}},
			},
			{
				Name: "create", Summary: "Create a thing", HTTPMethod: http.MethodPost, APIPath: "/things",
				Options: []Option{
					{Name: "name", Field: "name", In: inBody, Type: typeString, Required: true},
					{Name: "count", Field: "count", In: inBody, Type: typeInteger},
					{Name: "price", Field: "price", In: inBody, Type: typeNumber},
					{Name: "active", Field: "active", In: inBody, Type: typeBoolean},
				},
				HasRequestBody: true, RequestBodyRequired: []string{"name"},
			},
			{
				Name: "import", Summary: "Import things", HTTPMethod: http.MethodPost, APIPath: "/things/import",
				Options:        []Option{{Name: "source", Field: "source", In: inBody, Type: typeString, Required: true}},
				HasRequestBody: true, RequestBodyRequired: []string{"items", "source"},
			},
			{Name: "record", Summary: "Record a thing", HTTPMethod: http.MethodPost, APIPath: "/records", HasRequestBody: true},
			{Name: "track", Summary: "Track things", HTTPMethod: http.MethodPost, APIPath: "/things/track", HasRequestBody: true, IdentifiedByBody: true},
			{
				Name: "void", Summary: "Void a thing", HTTPMethod: http.MethodPost, APIPath: "/things/{id}/void",
				Arguments:           []Argument{{Name: "id"}},
				RequireConfirmation: "This operation cannot be reversed.",
			},
			{
				Name: "pdf", Summary: "Get a thing PDF", HTTPMethod: http.MethodGet, APIPath: "/things/{id}/pdf",
				Arguments: []Argument{{Name: "id"}}, Binary: true,
			},
		},
	}},
}

type recorded struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

type result struct {
	stdout string
	stderr string
	code   int
}

type CLISuite struct {
	suite.Suite
	server *httptest.Server
	// mu guards requests and responses, which the server's goroutines use. A
	// dropped connection gives the client no response to wait on, so reads lock too.
	mu        sync.Mutex
	requests  []recorded
	responses []http.HandlerFunc
	sleeps    []time.Duration
	stdin     string
	stdinTTY  bool
	stdoutTTY bool
	stderrTTY bool
	config    string
}

func TestCLI(t *testing.T) {
	suite.Run(t, new(CLISuite))
}

func (s *CLISuite) SetupTest() {
	s.reset()
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	// net/http itself replays an idempotent request whose reused connection drops,
	// which would hide the CLI's own retry of a dropped connection.
	s.server.Client().Transport.(*http.Transport).DisableKeepAlives = true
	home := s.T().TempDir()
	s.T().Setenv("HOME", home)
	s.T().Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	s.T().Setenv("AppData", filepath.Join(home, "AppData"))
	path, err := configPath()
	s.Require().NoError(err)
	s.config = path
	s.T().Setenv(envAPIKey, envKey)
	s.T().Setenv(envBaseURL, s.server.URL)
	s.T().Setenv(envAPIVersion, "")
}

// reset clears what one run records, for a table of runs in one test.
func (s *CLISuite) reset() {
	s.mu.Lock()
	s.requests, s.responses = nil, nil
	s.mu.Unlock()
	s.sleeps = nil
	s.stdin, s.stdinTTY, s.stdoutTTY, s.stderrTTY = "", false, false, false
}

func (s *CLISuite) TearDownTest() {
	s.server.Close()
}

// respond queues handlers for the next requests; after the queue, the server answers 200 {}.
func (s *CLISuite) respond(handlers ...http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, handlers...)
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_"+strconv.Itoa(code))
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// withRetryAfter is status with a Retry-After header.
func withRetryAfter(code int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		status(code, `{}`)(w, r)
	}
}

// hang answers when the client gives up and closes the connection. After 5 s it
// answers 200, so a client that never gives up fails the test instead of hanging it.
func hang(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// dropConnection closes the connection without a response.
func dropConnection(w http.ResponseWriter, _ *http.Request) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// sent returns the requests the server received.
func (s *CLISuite) sent() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func (s *CLISuite) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, recorded{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body)})
	handler := status(http.StatusOK, `{}`)
	if len(s.responses) > 0 {
		handler, s.responses = s.responses[0], s.responses[1:]
	}
	s.mu.Unlock()
	handler(w, r)
}

func (s *CLISuite) run(args ...string) result {
	var stdout, stderr bytes.Buffer
	code := Run(testManifest, Env{
		Stdin:            strings.NewReader(s.stdin),
		Stdout:           &stdout,
		Stderr:           &stderr,
		StdinIsTerminal:  s.stdinTTY,
		StdoutIsTerminal: s.stdoutTTY,
		StderrIsTerminal: s.stderrTTY,
		HTTPClient:       s.server.Client(),
		Sleep:            func(d time.Duration) { s.sleeps = append(s.sleeps, d) },
	}, args)
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func (s *CLISuite) errorOutput(res result) errorOutput {
	var out errorOutput
	s.Require().NoError(json.Unmarshal([]byte(res.stderr), &out), res.stderr)
	return out
}

func (s *CLISuite) headers(name string) []string {
	values := []string{}
	for _, r := range s.sent() {
		values = append(values, r.header.Get(name))
	}
	return values
}

func (s *CLISuite) queries() []string {
	values := []string{}
	for _, r := range s.sent() {
		values = append(values, r.query)
	}
	return values
}

func (s *CLISuite) TestSettingsPrecedence() {
	s.stdin = "key_config.secret\n"
	s.Require().Equal(0, s.run("login").code)
	s.T().Setenv(envAPIKey, " \n")
	s.run("things", "get", "a")
	s.T().Setenv(envAPIKey, envKey+"\n")
	s.run("things", "get", "a")
	s.T().Setenv(envAPIVersion, "2025-03-03")
	s.run("things", "get", "a", "--api-key", "  key_flag.secret\t", "--api-version", "2025-04-04")
	s.run("things", "get", "a")

	s.Equal([]string{"Bearer key_config.secret", "Bearer " + envKey, "Bearer key_flag.secret", "Bearer " + envKey}, s.headers("Authorization"))
	s.Equal([]string{"2026-04-01", "2026-04-01", "2025-04-04", "2025-03-03"}, s.headers("Alguna-Version"))
	s.Equal("/things/a", s.sent()[0].path)

	s.T().Setenv(envBaseURL, "http://127.0.0.1:1")
	s.Equal(0, s.run("things", "get", "a", "--base-url", s.server.URL+"/").code)
	s.Equal("/things/a", s.sent()[len(s.sent())-1].path, "the flag wins, without its trailing slash")
	s.T().Setenv(envBaseURL, "")
	s.Contains(s.run("things", "get", "a", "--dry-run").stdout, `"url": "https://api.alguna.io/things/a"`)
}

func (s *CLISuite) TestAPIKeyErrors() {
	for _, tc := range []struct{ name, key, flag, want string }{
		{name: "no key", want: "no API key: pass --api-key, set " + envAPIKey + ", or run alguna login"},
		{name: "a space", key: "key abc", want: "$" + envAPIKey + " contains whitespace or control"},
		{name: "a newline", key: "key\nabc", want: "$" + envAPIKey + " contains whitespace or control"},
		{name: "a control character", key: "key\x01abc", want: "$" + envAPIKey + " contains whitespace or control"},
		{name: "a space in the flag", key: envKey, flag: "a b", want: "--api-key contains whitespace or control"},
	} {
		s.Run(tc.name, func() {
			s.reset()
			s.T().Setenv(envAPIKey, tc.key)
			args := []string{"things", "get", "a"}
			if tc.flag != "" {
				args = append(args, "--api-key", tc.flag)
			}
			res := s.run(args...)
			s.Equal(1, res.code)
			s.Contains(s.errorOutput(res).Error, tc.want)
			s.NotContains(res.stderr, "abc")
			s.Empty(s.sent())
		})
	}
}

func (s *CLISuite) TestRetries() {
	for _, tc := range []struct {
		name      string
		responses []http.HandlerFunc
		args      []string
		code      int
		requests  int
		waits     []time.Duration
	}{
		{"a GET backs off", []http.HandlerFunc{status(503, `{}`), status(502, `{}`)}, []string{"get", "a"}, 0, 3, []time.Duration{time.Second, 2 * time.Second}},
		{"a GET gives up after three tries", []http.HandlerFunc{status(503, `{}`), status(503, `{}`), status(503, `{}`)}, []string{"get", "a"}, exitServerError, 3, []time.Duration{time.Second, 2 * time.Second}},
		{"a 4xx is not retried", []http.HandlerFunc{status(404, `{}`)}, []string{"get", "a"}, exitClientError, 1, nil},
		{"a write is retried", []http.HandlerFunc{status(503, `{}`)}, []string{"create", "--name", "a"}, 0, 2, []time.Duration{time.Second}},
		{"a write the spec does not mark idempotent is retried", []http.HandlerFunc{status(504, `{}`)}, []string{"record"}, 0, 2, []time.Duration{time.Second}},
		{"a GET retries a dropped connection", []http.HandlerFunc{dropConnection}, []string{"get", "a"}, 0, 2, []time.Duration{time.Second}},
		{"a write retries a dropped connection", []http.HandlerFunc{dropConnection}, []string{"record"}, 0, 2, []time.Duration{time.Second}},
		{"a GET gives up after three dropped connections", []http.HandlerFunc{dropConnection, dropConnection, dropConnection}, []string{"get", "a"}, exitServerError, 3, []time.Duration{time.Second, 2 * time.Second}},
		{"an attempt that times out is retried", []http.HandlerFunc{hang}, []string{"get", "a", "--timeout", "20ms"}, 0, 2, []time.Duration{time.Second}},
		{"--max-retries 0 turns retries off", []http.HandlerFunc{status(503, `{}`)}, []string{"get", "a", "--max-retries", "0"}, exitServerError, 1, nil},
		{"--max-retries sets the retries", []http.HandlerFunc{status(503, `{}`), status(503, `{}`), status(503, `{}`), status(503, `{}`), status(503, `{}`)}, []string{"get", "a", "--max-retries", "4"},
			exitServerError, 5, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}},
		{"a 409 with Retry-After waits for it", []http.HandlerFunc{withRetryAfter(409, "5")}, []string{"create", "--name", "a"}, 0, 2, []time.Duration{5 * time.Second}},
		{"a 409 without Retry-After is a conflict", []http.HandlerFunc{status(409, `{}`)}, []string{"create", "--name", "a"}, exitClientError, 1, nil},
		{"Retry-After on a 429 replaces the backoff", []http.HandlerFunc{status(503, `{}`), withRetryAfter(429, "1")}, []string{"get", "a"}, 0, 3, []time.Duration{time.Second, time.Second}},
		{"Retry-After is capped", []http.HandlerFunc{withRetryAfter(503, "3600")}, []string{"get", "a"}, 0, 2, []time.Duration{maxRetryWait}},
		{"Retry-After on a 502 waits for it", []http.HandlerFunc{withRetryAfter(502, "30")}, []string{"get", "a"}, 0, 2, []time.Duration{30 * time.Second}},
	} {
		s.Run(tc.name, func() {
			s.reset()
			s.respond(tc.responses...)
			s.Equal(tc.code, s.run(append([]string{"things"}, tc.args...)...).code)
			s.Len(s.sent(), tc.requests)
			// A backoff wait has ±10% jitter; a Retry-After wait is exact.
			s.Require().Len(s.sleeps, len(tc.waits))
			for i, want := range tc.waits {
				s.InDelta(want, s.sleeps[i], float64(want/10), "wait %d", i+1)
			}
		})
	}
}

func (s *CLISuite) TestExpiredContextIsNotRetried() {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	u, err := url.Parse(s.server.URL + "/things/a")
	s.Require().NoError(err)
	t := transport{client: s.server.Client(), sleep: func(d time.Duration) { s.sleeps = append(s.sleeps, d) }, timeout: defaultTimeout, maxRetries: defaultMaxRetries}
	_, err = t.send(ctx, &request{method: http.MethodGet, url: u, header: http.Header{}})
	var netErr *networkError
	s.Require().ErrorAs(err, &netErr)
	s.ErrorIs(err, context.DeadlineExceeded)
	s.Empty(s.sleeps)
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		wait  time.Duration
		ok    bool
	}{
		{"5", 5 * time.Second, true},
		{"0", 0, true},
		{"3600", maxRetryWait, true},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{now.Add(time.Hour).Format(http.TimeFormat), maxRetryWait, true},
		{"", 0, false},
		{"-1", 0, false},
		{"soon", 0, false},
	} {
		wait, ok := retryAfter(tc.value, now)
		assert.Equal(t, tc.ok, ok, tc.value)
		assert.Equal(t, tc.wait, wait, tc.value)
	}
}

// TestBackoff checks the wait when no Retry-After applies: 1 s, doubling up to a
// minute, with ±10% jitter.
func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		retry uint
		wait  time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {7, maxRetryWait}, {100, maxRetryWait}} {
		waits := map[time.Duration]bool{}
		for range 50 {
			wait := retryWait(tc.retry, http.Header{}, time.Now())
			assert.InDelta(t, tc.wait, wait, float64(tc.wait/10), "retry %d", tc.retry)
			assert.LessOrEqual(t, wait, maxRetryWait, "retry %d", tc.retry)
			waits[wait] = true
		}
		if tc.wait < maxRetryWait {
			assert.Greater(t, len(waits), 1, "retry %d waits the same every time", tc.retry)
		}
	}
}

func (s *CLISuite) TestIdempotencyKey() {
	s.respond(status(503, `{}`))
	s.run("things", "create", "--name", "a")
	s.run("things", "create", "--name", "a")
	s.run("things", "create", "--name", "a", "--idempotency-key", "key_1")
	s.run("things", "create", "--name", "a", "--idempotency-key", "")
	s.run("things", "record")
	s.run("things", "void", "a", "--yes")
	s.run("things", "get", "a")

	uuid := `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`
	keys := s.headers(idempotencyKeyHeader)
	s.Require().Len(keys, 8)
	s.Regexp(uuid, keys[0])
	s.Equal(keys[0], keys[1], "a retry sends the same key")
	s.NotEqual(keys[0], keys[2], "each run gets a new key")
	s.Equal("key_1", keys[3])
	for _, key := range keys[4:7] {
		s.Regexp(uuid, key, "every write gets a key")
	}
	s.NotContains(s.sent()[7].header, idempotencyKeyHeader, "a GET gets no key")
}

// TestIdentifiedByBody: a method the API identifies by its body sends no key of
// its own and is still retried, with the same body.
func (s *CLISuite) TestIdentifiedByBody() {
	body := `{"unique_id":"u1"}`
	s.respond(status(503, `{}`), dropConnection)
	s.Require().Equal(0, s.run("things", "track", "--body", body).code)
	s.respond(status(503, `{}`))
	s.Require().Equal(0, s.run("things", "track", "--body", body, "--idempotency-key", "key_1").code)

	sent := s.sent()
	s.Require().Len(sent, 5)
	for i, r := range sent {
		s.JSONEq(body, r.body)
		if i < 3 {
			s.NotContains(r.header, idempotencyKeyHeader, "attempt %d", i+1)
		}
	}
	s.Equal([]string{"key_1", "key_1"}, s.headers(idempotencyKeyHeader)[3:], "the caller's key on every retry")
	s.Contains(s.run("things", "track", "--help").stdout, "default: none, because the API identifies this request by its body")
}

var thingItems = []string{`{"id":"t1"}`, `{"id":"t2"}`, `{"id":"t3"}`, `{"id":"t4"}`, `{"id":"t5"}`}

// thingPages serves thingItems with the page size capped at maxLimit.
func thingPages(maxLimit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		limit = min(limit, maxLimit)
		data := []string{}
		if offset < len(thingItems) {
			data = thingItems[offset:min(offset+limit, len(thingItems))]
		}
		status(200, `{"data":[`+strings.Join(data, ",")+`],"pagination":{}}`)(w, r)
	}
}

func lines(items []string) string { return strings.Join(items, "\n") + "\n" }

func (s *CLISuite) TestAll() {
	for _, tc := range []struct {
		name    string
		server  http.HandlerFunc
		args    []string
		stdout  string
		queries []string
	}{
		{"clamps the page size and stops on a short page", thingPages(2), []string{"list", "--status", "active"}, lines(thingItems),
			[]string{"limit=2&offset=0&status=active", "limit=2&offset=2&status=active", "limit=2&offset=4&status=active"}},
		{"starts at --offset", thingPages(2), []string{"list", "--offset", "2"}, lines(thingItems[2:]), []string{"limit=2&offset=2", "limit=2&offset=4"}},
		{"--limit caps the total", thingPages(2), []string{"list", "--limit", "3"}, lines(thingItems[:3]), []string{"limit=2&offset=0", "limit=2&offset=2"}},
		{"a small --limit is the page size", thingPages(2), []string{"list", "--limit", "1", "--offset", "2"}, lines(thingItems[2:3]), []string{"limit=1&offset=2"}},
		{"a large --limit returns everything", thingPages(2), []string{"list", "--limit", "50"}, lines(thingItems),
			[]string{"limit=2&offset=0", "limit=2&offset=2", "limit=2&offset=4"}},
	} {
		s.Run(tc.name, func() {
			s.reset()
			for range 2 * len(thingItems) {
				s.respond(tc.server)
			}
			res := s.run(append([]string{"things"}, append(tc.args, "--all", "-o", "ndjson")...)...)
			s.Require().Equal(0, res.code, res.stderr)
			s.Equal(tc.stdout, res.stdout)
			s.Equal(tc.queries, s.queries())
		})
	}
}

func (s *CLISuite) TestAllWithJSONPrintsOneArray() {
	for range 3 {
		s.respond(thingPages(2))
	}
	s.respond(status(200, `{"data":[]}`), status(200, `{"data":[{"id":"a&b<c>"}]}`))
	s.respond(thingPages(2), status(500, `{"detail":"boom"}`))

	var items []map[string]string
	res := s.run("things", "list", "--all")
	s.Require().NoError(json.Unmarshal([]byte(res.stdout), &items), res.stdout)
	s.Len(items, 5)
	s.True(strings.HasPrefix(res.stdout, "[\n  {"), res.stdout)
	s.Equal("[]\n", s.run("things", "list", "--all").stdout)
	s.Equal("[\n  {\n    \"id\": \"a&b<c>\"\n  }\n]\n", s.run("things", "list", "--all").stdout, "no HTML escaping")

	res = s.run("things", "list", "--all")
	s.Equal(exitServerError, res.code)
	s.Empty(res.stdout, "a failed page leaves no partial array")
}

func (s *CLISuite) TestOutput() {
	s.respond(
		status(200, `{"data":[{"id":"t1"}],"pagination":{"per_page":1}}`),
		status(200, `{"data":[{"id":"t1"},{"id":"t2"}]}`),
		status(200, `{"id": "t1",  "name": "a"}`),
	)
	res := s.run("things", "list", "--limit", "1", "--offset", "3", "--tag-ids", "a,b", "--tag-ids", "c", "--include-archived")
	s.Equal("{\n  \"data\": [\n    {\n      \"id\": \"t1\"\n    }\n  ],\n  \"pagination\": {\n    \"per_page\": 1\n  }\n}\n", res.stdout)
	s.Equal("include_archived=true&limit=1&offset=3&tag_ids=a%2Cb%2Cc", s.sent()[0].query)
	s.Equal("{\"id\":\"t1\"}\n{\"id\":\"t2\"}\n", s.run("things", "list", "-o", "ndjson").stdout)
	s.Equal("{\"id\":\"t1\",\"name\":\"a\"}\n", s.run("things", "get", "t1", "--output", "ndjson").stdout)
}

func (s *CLISuite) TestBody() {
	file := filepath.Join(s.T().TempDir(), "body.json")
	s.Require().NoError(os.WriteFile(file, []byte(`{"name":"from file","extra":{"nested":true}}`), 0o600))
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"inline, numbers as written", []string{"create", "--body", `{"name":"inline","price":1.50}`}, "", `{"name":"inline","price":1.50}`},
		{"from a file", []string{"create", "--body", "@" + file}, "", `{"extra":{"nested":true},"name":"from file"}`},
		{"from stdin", []string{"create", "--body", "-"}, `{"name":"from stdin"}`, `{"name":"from stdin"}`},
		{"field flags override --body", []string{"create", "--body", `{"name":"old","count":1,"note":"kept"}`, "--name", "new", "--count", "12345678901234567", "--price", "12.50", "--active"}, "",
			`{"active":true,"count":12345678901234567,"name":"new","note":"kept","price":12.50}`},
		{"no HTML escaping", []string{"create", "--body", `{}`, "--name", "A&B <c>"}, "", `{"name":"A&B <c>"}`},
		{"a required list in --body", []string{"import", "--body", `{"items":[{"id":"t1"}]}`, "--source", "csv"}, "", `{"items":[{"id":"t1"}],"source":"csv"}`},
		{"no flags sends an empty object", []string{"record"}, "", `{}`},
	} {
		s.Run(tc.name, func() {
			s.reset()
			s.stdin = tc.stdin
			res := s.run(append([]string{"things"}, tc.args...)...)
			s.Require().Equal(0, res.code, res.stderr)
			s.Require().Len(s.sent(), 1)
			s.Equal(tc.want, s.sent()[0].body)
			s.Equal("application/json", s.sent()[0].header.Get("Content-Type"))
		})
	}
}

// TestUsageErrors runs on a terminal: one line, then a pointer to the command's help.
func (s *CLISuite) TestUsageErrors() {
	for _, tc := range []struct {
		args []string
		want string
		help string
	}{
		{[]string{"things", "get"}, "missing required argument <id>", "alguna things get"},
		{[]string{"things", "get", "a", "b"}, `unexpected argument "b"`, "alguna things get"},
		{[]string{"things", "lookup"}, "missing required flags: --alias", "alguna things lookup"},
		{[]string{"things", "create", "--count", "1"}, "missing required fields: --name (or \"name\" in --body)", "alguna things create"},
		{[]string{"things", "create", "--body", `{"count":1}`, "--dry-run"}, "missing required fields: --name", "alguna things create"},
		{[]string{"things", "import"}, `missing required fields: "items" in --body, --source (or "source" in --body)`, "alguna things import"},
		{[]string{"things", "get", "a", "--nope"}, "unknown flag: --nope", "alguna things get"},
		{[]string{"things", "create", "--count", "1.5"}, `invalid argument "1.5" for "--count" flag: "1.5" is not an integer`, "alguna things create"},
		{[]string{"things", "create", "--price", "cheap"}, `invalid argument "cheap" for "--price" flag: "cheap" is not a number`, "alguna things create"},
		{[]string{"things", "create", "--body", `[1]`}, "--body must be a JSON object", "alguna things create"},
		{[]string{"things", "get", "a", "-o", "yaml"}, "--output must be json or ndjson", "alguna things get"},
		{[]string{"things", "get", "a", "--timeout", "0s"}, "--timeout must be above 0", "alguna things get"},
		{[]string{"things", "gett"}, `unknown command "gett" for "alguna things". Did you mean "get"?`, "alguna things"},
		{[]string{"thing"}, `unknown command "thing" for "alguna". Did you mean "things"?`, "alguna"},
		{[]string{"login", "--dry-run"}, "unknown flag: --dry-run", "alguna login"},
		{[]string{"logout", "--api-key", "x"}, "unknown flag: --api-key", "alguna logout"},
		{[]string{"version", "-o", "json"}, "unknown shorthand flag: 'o'", "alguna version"},
	} {
		s.Run(strings.Join(tc.args, " "), func() {
			s.reset()
			s.stderrTTY = true
			res := s.run(tc.args...)
			s.Equal(1, res.code)
			s.Empty(res.stdout)
			s.Contains(res.stderr, "Error: "+tc.want)
			s.True(strings.HasSuffix(res.stderr, "\nRun '"+tc.help+" --help' for usage.\n"), res.stderr)
			s.Empty(s.sent())
		})
	}
	s.NotContains(s.run("things", "zzzzzz").stderr, "Did you mean")
	s.Equal(1, s.run("things", "get", "a", "--base-url", "ftp://example.com").code)
	s.Empty(s.sent())
	s.NoFileExists(s.config)
}

func (s *CLISuite) TestAPIErrors() {
	for _, tc := range []struct {
		name     string
		response http.HandlerFunc
		code     int
		want     string
	}{
		{"problem detail", status(422, `{"status":422,"detail":"currency must be a 3-letter code","title":"Unprocessable"}`), exitClientError, "Error: currency must be a 3-letter code (HTTP 422, request req_422)\n"},
		{"title before message", status(409, `{"title":"Conflict","message":"the gateway text"}`), exitClientError, "Error: Conflict (HTTP 409, request req_409)\n"},
		{"gateway message and a login hint", status(401, `{"code":"unauthenticated","message":"invalid API key"}`), exitClientError, "Error: invalid API key (HTTP 401, request req_401)\nCheck your API key, or run 'alguna login'.\n"},
		{"no message", status(404, `{}`), exitClientError, "Error: Not Found (HTTP 404, request req_404)\n"},
		{"plain text", status(500, `upstream exploded`), exitServerError, "Error: upstream exploded (HTTP 500, request req_500)\n"},
		{"an HTML page", status(400, "<html>bad request</html>"), exitClientError, "Error: Bad Request (HTTP 400, request req_400)\n"},
	} {
		s.Run(tc.name, func() {
			s.reset()
			s.stderrTTY = true
			s.respond(tc.response)
			res := s.run("things", "get", "a")
			s.Equal(tc.code, res.code)
			s.Equal(tc.want, res.stderr)
		})
	}

	s.reset()
	s.respond(status(404, `{"status":404,"detail":"thing not found"}`), status(500, `upstream exploded`))
	out := s.errorOutput(s.run("things", "get", "missing"))
	s.Equal(errorOutput{Error: "HTTP 404 Not Found", Status: 404, RequestID: "req_404", Response: json.RawMessage(`{"status":404,"detail":"thing not found"}`)}, out)
	s.Equal(`"upstream exploded"`, string(s.errorOutput(s.run("things", "get", "a")).Response))

	s.stderrTTY = true
	s.respond(status(404, `{}`))
	s.Equal(404, s.errorOutput(s.run("things", "get", "a", "--json-errors")).Status)
	s.Contains(s.errorOutput(s.run("things", "gett", "--json-errors")).Error, "unknown command")

	res := s.run("things", "get", "a", "--base-url", "http://127.0.0.1:1")
	s.Equal(exitServerError, res.code, "a network error")
	s.NotContains(res.stderr, envKey)
	s.NotContains(res.stderr, "Run '", "a runtime error does not point to help")

	s.respond(hang)
	res = s.run("things", "get", "a", "--timeout", "20ms", "--max-retries", "0")
	s.Equal(exitServerError, res.code, "a timeout")
	s.True(strings.HasSuffix(res.stderr, "\nUse --timeout to wait longer for each attempt (default 1m0s).\n"), res.stderr)
}

func (s *CLISuite) TestDryRunSendsNothingAndRedactsTheKey() {
	res := s.run("things", "create", "--name", "Acme", "--api-key", "key_flag.secret_flag", "--dry-run")
	s.Require().Equal(0, res.code, res.stderr)
	s.NotContains(res.stdout, "key_flag.secret_flag")
	s.NotContains(res.stdout, envKey)

	var out dryRunOutput
	s.Require().NoError(json.Unmarshal([]byte(res.stdout), &out))
	s.Equal(http.MethodPost, out.Method)
	s.Equal(s.server.URL+"/things", out.URL)
	s.Equal("Bearer "+redacted, out.Headers["Authorization"])
	s.Equal("2026-04-01", out.Headers["Alguna-Version"])
	s.NotEmpty(out.Headers[idempotencyKeyHeader])
	s.JSONEq(`{"name":"Acme"}`, string(out.Body))

	s.Equal(0, s.run("things", "void", "a", "--dry-run").code, "a dry run needs no confirmation")
	s.Empty(s.sent())
}

func (s *CLISuite) TestConfirmation() {
	res := s.run("things", "void", "a/b")
	s.Equal(1, res.code)
	s.Equal("Void thing a/b? This operation cannot be reversed. Pass --yes to confirm (stdin is not a terminal).", s.errorOutput(res).Error)

	s.stdinTTY = true
	s.stdin = "n\n"
	res = s.run("things", "void", "a")
	s.Equal(1, res.code)
	s.True(strings.HasPrefix(res.stderr, "Void thing a? This operation cannot be reversed. [y/N] "), res.stderr)
	s.Empty(s.sent())

	s.stdin = "y\n"
	s.Equal(0, s.run("things", "void", "a").code)
	s.stdinTTY = false
	s.Equal(0, s.run("things", "void", "a", "--yes").code)
	s.run("things", "get", "a/b c")
	s.Equal([]string{"/things/a/void", "/things/a/void", "/things/a%2Fb%20c"}, []string{s.sent()[0].path, s.sent()[1].path, s.sent()[2].path})
}

func (s *CLISuite) TestBinaryResponse() {
	pdf := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-1.7 bytes")
	}
	s.respond(pdf, pdf)

	out := filepath.Join(s.T().TempDir(), "thing.pdf")
	res := s.run("things", "pdf", "a", "--out", out)
	s.Require().Equal(0, res.code, res.stderr)
	s.Empty(res.stdout)
	data, err := os.ReadFile(out)
	s.Require().NoError(err)
	s.Equal("%PDF-1.7 bytes", string(data))
	s.Equal("%PDF-1.7 bytes", s.run("things", "pdf", "a").stdout)

	s.stdoutTTY = true
	res = s.run("things", "pdf", "a")
	s.Equal(1, res.code)
	s.Contains(s.errorOutput(res).Error, "--out")
	s.Len(s.sent(), 2)
}

func (s *CLISuite) TestLoginAndLogout() {
	// login sets 0600 also on a file that exists with another mode.
	s.Require().NoError(os.MkdirAll(filepath.Dir(s.config), 0o700))
	s.Require().NoError(os.WriteFile(s.config, []byte(`{}`), 0o644))
	s.stdin = "  key_login.secret  \n"
	res := s.run("login")
	s.Require().Equal(0, res.code, res.stderr)
	s.Contains(res.stderr, "API key: ")
	s.NotContains(res.stderr, "key_login.secret")
	s.Contains(res.stderr, "$"+envAPIKey+" is set and takes precedence")
	info, err := os.Stat(s.config)
	s.Require().NoError(err)
	s.Equal(os.FileMode(0o600), info.Mode().Perm())
	cfg, err := loadConfig()
	s.Require().NoError(err)
	s.Equal("key_login.secret", cfg.APIKey)

	res = s.run("login", "--api-key", "  key_flag.secret \n")
	s.Require().Equal(0, res.code, res.stderr)
	s.NotContains(res.stderr, "API key:", "--api-key does not prompt")
	cfg, _ = loadConfig()
	s.Equal("key_flag.secret", cfg.APIKey)

	res = s.run("logout")
	s.Contains(res.stderr, "Removed the saved API key")
	s.Contains(res.stderr, "$"+envAPIKey+" is still set")
	s.NoFileExists(s.config)
	s.T().Setenv(envAPIKey, "")
	res = s.run("logout")
	s.Contains(res.stderr, "nothing to remove")
	s.NotContains(res.stderr, "Warning")

	for _, args := range [][]string{{"login", "--api-key", "key abc"}, {"login"}} {
		s.stdin = "\n"
		res = s.run(args...)
		s.Equal(1, res.code, "%v", args)
		s.NotContains(res.stderr, "key abc")
	}
	s.NoFileExists(s.config)
}

func (s *CLISuite) TestHelp() {
	for _, args := range [][]string{{}, {"things"}} {
		res := s.run(args...)
		s.Equal(0, res.code, "%v", args)
		s.Contains(res.stdout, "Usage:")
		s.Empty(res.stderr)
	}
	help := s.run("things", "list", "--help").stdout
	s.Contains(help, "(maximum 2 per request); with --all, the most items to return in total")
	s.Contains(help, "such as 30s or 2m (default 1m0s)")
	s.Contains(help, "0 turns retries off (default 2)")
	s.NotContains(help, "--idempotency-key", "a GET sends no key")
	s.True(strings.HasSuffix(help, "\nExample:\n  alguna things list --all\n"), help)
	s.Contains(s.run("things", "get", "--help").stdout, "GET /things/{id}\n\nArguments:\n  <id>  The thing ID\n\nUsage:")
	s.Contains(s.run("things", "void", "--help").stdout, "--yes")
	s.Contains(s.run("things", "create", "--help").stdout, "--idempotency-key")
	s.Contains(s.run("version").stdout, "default API version 2026-04-01")
}
