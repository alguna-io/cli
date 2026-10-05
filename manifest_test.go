package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	kebabName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	pathParam = regexp.MustCompile(`\{([^}]+)\}`)
)

// The manifest is beside main.go in the generated module. The source module has
// none until generation runs, so its tests also look in the generated output.
var (
	commandDir         = filepath.Join("cmd", "alguna")
	generatedManifests = []string{
		filepath.Join(commandDir, "manifest.json"),
		filepath.Join("..", "..", "out", "cli", commandDir, "manifest.json"),
	}
)

// generatedManifest returns the path and the bytes of the manifest the binary embeds.
func generatedManifest(t *testing.T) (string, []byte) {
	t.Helper()
	for _, path := range generatedManifests {
		data, err := os.ReadFile(path)
		if err == nil {
			return path, data
		}
		require.ErrorIs(t, err, fs.ErrNotExist)
	}
	t.Fatalf("no generated manifest at %s: run `pnpm -C sdks generate cli` first", strings.Join(generatedManifests, " or "))
	return "", nil
}

// TestManifest checks the generated manifest: it has no field the Manifest type
// does not declare, every name is a valid command or flag, and every method's
// command sends its HTTP method to its path. build.ts fails when a spec operation
// has no method, so together they cover every operation of the spec.
func TestManifest(t *testing.T) {
	_, data := generatedManifest(t)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var m Manifest
	require.NoError(t, decoder.Decode(&m))
	require.NotEmpty(t, m.BaseURL)
	require.NotEmpty(t, m.APIVersion)

	// newRootCommand panics when two flags of one command share a name, such as an
	// option named --output or --body. An option named like the flags cobra adds
	// (--help) or that the root passes down (--json-errors) would hide them instead.
	reserved := []string{"help", flagJSONErrors}
	commands := map[string]bool{}
	for _, command := range m.Commands {
		assert.Regexp(t, kebabName, command.Name)
		assert.False(t, commands[command.Name], "repeated command %s", command.Name)
		assert.NotEmpty(t, command.Description, command.Name)
		commands[command.Name] = true
		methods := map[string]bool{}
		for _, method := range command.Methods {
			where := command.Name + " " + method.Name
			assert.Regexp(t, kebabName, method.Name, where)
			assert.False(t, methods[method.Name], "repeated command %s", where)
			methods[method.Name] = true
			assert.Contains(t, []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, method.HTTPMethod, where)
			var params, arguments []string
			for _, match := range pathParam.FindAllStringSubmatch(method.APIPath, -1) {
				params = append(params, match[1])
			}
			for _, a := range method.Arguments {
				arguments = append(arguments, a.Name)
			}
			assert.Equal(t, params, arguments, "%s: one argument per path parameter, in order", where)
			for _, o := range method.Options {
				assert.Regexp(t, kebabName, o.Name, where)
				assert.NotContains(t, reserved, o.Name, where)
				assert.Contains(t, []string{inQuery, inBody}, o.In, where)
				assert.Contains(t, []OptionType{typeString, typeInteger, typeNumber, typeBoolean, typeStringList}, o.Type, where)
			}
			if p := method.Pagination; p != nil {
				assert.True(t, slices.ContainsFunc(method.Options, func(o Option) bool { return o.Field == p.Limit }), "%s: no --%s", where, p.Limit)
				assert.Positive(t, p.MaxLimit, "%s: --all needs a page size", where)
			}
		}
	}

	t.Setenv(envAPIKey, "key_test.secret")
	t.Setenv(envBaseURL, "")
	t.Setenv(envAPIVersion, "")
	var identifiedByBody []string
	for _, command := range m.Commands {
		for _, method := range command.Methods {
			if method.IdentifiedByBody {
				identifiedByBody = append(identifiedByBody, command.Name+" "+method.Name)
			}
			args, path := sampleArgs(command, method)
			var stdout, stderr bytes.Buffer
			code := Run(m, Env{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}, append(args, "--dry-run"))
			if !assert.Equal(t, 0, code, "alguna %s\n%s", strings.Join(args, " "), stderr.String()) {
				continue
			}
			var request dryRunOutput
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &request))
			assert.Equal(t, method.HTTPMethod, request.Method, method.OperationID)
			assert.Equal(t, m.BaseURL+path, strings.SplitN(request.URL, "?", 2)[0], method.OperationID)
			_, hasKey := request.Headers[idempotencyKeyHeader]
			assert.Equal(t, method.HTTPMethod != http.MethodGet && !method.IdentifiedByBody, hasKey, "%s: Idempotency-Key", method.OperationID)
		}
	}
	// The operations the spec marks x-alguna-identified-by-body.
	assert.Subset(t, identifiedByBody, []string{"credits check", "credits track", "events ingest"})
}

// sampleArgs is a command line that sets every argument, required option and
// required body property, and the path it must send to.
func sampleArgs(command Command, method Method) ([]string, string) {
	args := []string{command.Name, method.Name}
	path := method.APIPath
	for i, a := range method.Arguments {
		value := fmt.Sprintf("p%d", i)
		args = append(args, value)
		path = strings.Replace(path, "{"+a.Name+"}", value, 1)
	}
	for _, o := range method.Options {
		if o.Required && o.In == inQuery {
			args = append(args, "--"+o.Name, sampleValue(o))
		}
	}
	if len(method.RequestBodyRequired) > 0 {
		// The runtime checks that a required property is present, not its value.
		body := map[string]any{}
		for _, field := range method.RequestBodyRequired {
			body[field] = nil
		}
		data, _ := json.Marshal(body)
		args = append(args, "--body", string(data))
	}
	return args, path
}

func sampleValue(o Option) string {
	switch {
	case len(o.Enum) > 0:
		return o.Enum[0]
	case o.Type == typeInteger || o.Type == typeNumber:
		return "1"
	case o.Type == typeBoolean:
		return "true"
	default:
		return "x"
	}
}

// TestBinary builds cmd/alguna and runs it against a fake API: the embedded
// manifest reaches the binary, and the exit code reaches the shell.
func TestBinary(t *testing.T) {
	manifestPath, data := generatedManifest(t)
	var m Manifest
	require.NoError(t, json.Unmarshal(data, &m))

	binary := filepath.Join(t.TempDir(), "alguna")
	build := []string{"build", "-o", binary}
	// In the source module, -overlay builds cmd/alguna as generation lays it out.
	if _, err := os.Stat(filepath.Join(commandDir, "main.go")); errors.Is(err, fs.ErrNotExist) {
		overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{
			filepath.Join(commandDir, "main.go"):       filepath.Join(commandDir, "main.go.tmpl"),
			filepath.Join(commandDir, "manifest.json"): manifestPath,
		}})
		require.NoError(t, err)
		overlayPath := filepath.Join(t.TempDir(), "overlay.json")
		require.NoError(t, os.WriteFile(overlayPath, overlay, 0o600))
		build = append(build, "-overlay", overlayPath)
	}
	output, err := exec.Command("go", append(build, "./"+filepath.ToSlash(commandDir))...).CombinedOutput()
	require.NoError(t, err, string(output))

	var requests []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/customers" {
			_, _ = io.WriteString(w, `{"data":[{"id":"cus_1"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"customer not found"}`)
	}))
	defer server.Close()

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binary, args...)
		cmd.Env = []string{"HOME=" + t.TempDir(), "ALGUNA_API_KEY=key_test.secret", "ALGUNA_BASE_URL=" + server.URL}
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), exit.ExitCode()
		}
		require.NoError(t, err)
		return stdout.String(), 0
	}

	stdout, code := run("customers", "list", "--limit", "1", "-o", "ndjson")
	assert.Equal(t, 0, code)
	assert.Equal(t, `{"id":"cus_1"}`+"\n", stdout)
	_, code = run("customers", "get", "cus_missing")
	assert.Equal(t, exitClientError, code)

	require.Len(t, requests, 2)
	assert.Equal(t, "limit=1", requests[0].URL.RawQuery)
	assert.Equal(t, "Bearer key_test.secret", requests[0].Header.Get("Authorization"))
	assert.Equal(t, m.APIVersion, requests[0].Header.Get("Alguna-Version"))
}

// TestExamples checks every alguna command line in the README's sh blocks and in
// the help epilogues: the command exists, it has the right number of arguments,
// and its flags exist and parse.
func TestExamples(t *testing.T) {
	_, data := generatedManifest(t)
	var m Manifest
	require.NoError(t, json.Unmarshal(data, &m))
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)

	var examples []string
	for _, block := range regexp.MustCompile("(?s)```sh\n(.*?)```").FindAllStringSubmatch(string(readme), -1) {
		examples = append(examples, strings.Split(block[1], "\n")...)
	}
	for _, command := range m.Commands {
		for _, method := range command.Methods {
			examples = append(examples, strings.Split(method.Epilogue, "\n")...)
		}
	}
	checked := 0
	for _, line := range examples {
		args := commandWords(line)
		if len(args) == 0 || args[0] != "alguna" {
			continue
		}
		checked++
		cmd, rest, err := newRootCommand(m, &Env{}).Find(args[1:])
		if !assert.NoError(t, err, line) || !assert.False(t, cmd.HasSubCommands(), "%s: not a command", line) {
			continue
		}
		if assert.NoError(t, cmd.ParseFlags(rest), line) {
			assert.NoError(t, cmd.ValidateArgs(cmd.Flags().Args()), line)
		}
	}
	assert.NotZero(t, checked, "no examples found")
}

var shellWord = regexp.MustCompile(`"[^"]*"|'[^']*'|\S+`)

// commandWords splits a shell line into words, without quotes, up to the first
// redirect, pipe or comment.
func commandWords(line string) []string {
	var words []string
	for _, word := range shellWord.FindAllString(line, -1) {
		if strings.ContainsRune(">|#;&", rune(word[0])) {
			break
		}
		words = append(words, strings.Trim(word, `"'`))
	}
	return words
}
