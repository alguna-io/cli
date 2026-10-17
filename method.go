package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type outputFormat string

const (
	outputJSON   outputFormat = "json"
	outputNDJSON outputFormat = "ndjson"
)

// apiFlags are the flags of every API command. They are not on the root
// command, so login, logout and version do not take flags they would ignore.
type apiFlags struct {
	apiKey     string
	baseURL    string
	apiVersion string
	output     string
	dryRun     bool
	timeout    time.Duration
	maxRetries uint
}

func (f *apiFlags) bind(flags *pflag.FlagSet, m Manifest) {
	flags.StringVar(&f.apiKey, "api-key", "", "API key (default: $"+envAPIKey+", then the key saved by alguna login)")
	flags.StringVar(&f.baseURL, "base-url", "", "API base URL (default: $"+envBaseURL+", then "+m.BaseURL+")")
	flags.StringVar(&f.apiVersion, "api-version", "", "Alguna-Version header (default: $"+envAPIVersion+", then "+m.APIVersion+")")
	flags.StringVarP(&f.output, "output", "o", string(outputJSON), "Output format: json (indented; one array with --all) or ndjson (one line per item)")
	flags.BoolVar(&f.dryRun, "dry-run", false, "Print the request and send nothing")
	flags.DurationVar(&f.timeout, "timeout", defaultTimeout, "Time each attempt has to respond, such as 30s or 2m")
	flags.UintVar(&f.maxRetries, "max-retries", defaultMaxRetries, "Retries after a network error, a timeout or a status that a retry can fix; 0 turns retries off")
}

func (f *apiFlags) outputFormat() (outputFormat, error) {
	switch format := outputFormat(f.output); format {
	case outputJSON, outputNDJSON:
		return format, nil
	default:
		return "", usagef("--output must be json or ndjson, not %q", f.output)
	}
}

// methodOptions are the flags of one method command, and what its run reads.
type methodOptions struct {
	apiFlags
	env      *Env
	manifest Manifest
	method   Method

	query          []boundOption
	body           []boundOption
	bodyJSON       string
	yes            bool
	idempotencyKey string
	all            bool
	out            string
}

func methodCommand(m Manifest, method Method, env *Env) *cobra.Command {
	o := &methodOptions{env: env, manifest: m, method: method}
	use := method.Name
	for _, a := range method.Arguments {
		use += " <" + a.Name + ">"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: method.Summary,
		Long:  methodHelp(method),
		Args:  positionalArgs(method.Arguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd.Context(), args)
		},
	}
	if method.Epilogue != "" {
		cmd.Annotations = map[string]string{"epilogue": method.Epilogue}
	}

	flags := cmd.Flags()
	o.apiFlags.bind(flags, m)
	for _, option := range method.Options {
		if option.In == inQuery {
			o.query = append(o.query, bindOption(flags, option))
		} else {
			o.body = append(o.body, bindOption(flags, option))
		}
	}
	if method.HasRequestBody {
		flags.StringVar(&o.bodyJSON, "body", "", "Request body as a JSON object: inline JSON, @file, or - for stdin. Field flags override its fields")
	}
	if method.RequireConfirmation != "" {
		flags.BoolVar(&o.yes, "yes", false, "Do not ask for confirmation")
	}
	if method.HTTPMethod != http.MethodGet {
		defaultKey := "a new random key"
		if method.IdentifiedByBody {
			defaultKey = "none, because the API identifies this request by its body"
		}
		flags.StringVar(&o.idempotencyKey, "idempotency-key", "", "Idempotency-Key header, sent again on every retry (default: "+defaultKey+")")
	}
	if p := method.Pagination; p != nil {
		if flag := flags.Lookup(p.Limit); flag != nil {
			flag.Usage += fmt.Sprintf(" (maximum %d per request); with --all, the most items to return in total", p.MaxLimit)
		}
		flags.BoolVar(&o.all, "all", false, "Get every page. With -o json, print one JSON array; with -o ndjson, stream one line per item")
	}
	if method.Binary {
		flags.StringVar(&o.out, "out", "", "Write the file to this path (required when stdout is a terminal)")
	}
	return cmd
}

// methodHelp is the long help of a method command: its summary, description,
// HTTP route and arguments.
func methodHelp(method Method) string {
	help := method.Summary
	if method.Description != "" {
		help += "\n\n" + method.Description
	}
	help += fmt.Sprintf("\n\n%s %s", method.HTTPMethod, method.APIPath)
	if len(method.Arguments) == 0 {
		return help
	}
	var arguments strings.Builder
	w := tabwriter.NewWriter(&arguments, 0, 0, 2, ' ', 0)
	for _, a := range method.Arguments {
		fmt.Fprintf(w, "  <%s>\t%s\n", a.Name, a.Description)
	}
	_ = w.Flush()
	return help + "\n\nArguments:\n" + strings.TrimRight(arguments.String(), "\n")
}

// positionalArgs checks the arguments of a method command and names a missing one.
func positionalArgs(arguments []Argument) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < len(arguments) {
			missing := make([]string, 0, len(arguments)-len(args))
			for _, a := range arguments[len(args):] {
				missing = append(missing, "<"+a.Name+">")
			}
			noun := "argument"
			if len(missing) > 1 {
				noun = "arguments"
			}
			return usagef("missing required %s %s", noun, strings.Join(missing, " "))
		}
		if len(args) > len(arguments) {
			return usagef("unexpected argument %q", args[len(arguments)])
		}
		return nil
	}
}

func (o *methodOptions) run(ctx context.Context, args []string) error {
	format, err := o.outputFormat()
	if err != nil {
		return err
	}
	if o.timeout <= 0 {
		return usagef("--timeout must be above 0, such as 30s or 2m")
	}
	s, err := resolveSettings(o.manifest, o.apiFlags)
	if err != nil {
		return err
	}
	req, err := o.newRequest(s, args)
	if err != nil {
		return err
	}
	var pages *pager
	if o.all {
		pages = newPager(o.method.Pagination, o.query)
		pages.setQuery(req)
	}
	if o.dryRun {
		return writeDryRun(o.env.Stdout, req, s.apiKey != "")
	}
	if s.apiKey == "" {
		return errors.New("no API key: pass --api-key, set " + envAPIKey + ", or run alguna login")
	}
	if o.method.Binary && o.out == "" && o.env.StdoutIsTerminal {
		return usagef("this command returns a file: pass --out <file>, or redirect stdout")
	}
	if o.method.RequireConfirmation != "" && !o.yes {
		if err := confirm(o.env, o.method, args); err != nil {
			return err
		}
	}
	req.header.Set("Authorization", "Bearer "+s.apiKey)

	t := transport{client: o.env.HTTPClient, sleep: o.env.Sleep, timeout: o.timeout, maxRetries: o.maxRetries}
	if pages != nil {
		return pages.walk(ctx, t, req, format, o.env.Stdout)
	}
	body, err := t.send(ctx, req)
	if err != nil {
		return err
	}
	if o.method.Binary {
		return writeFile(o.env.Stdout, o.out, body)
	}
	return writeJSON(o.env.Stdout, format, o.method.Pagination, body)
}

// newRequest builds the request without the Authorization header, which run
// sets only after the dry-run exit.
func (o *methodOptions) newRequest(s settings, args []string) (*request, error) {
	path := o.method.APIPath
	for i, a := range o.method.Arguments {
		path = strings.Replace(path, "{"+a.Name+"}", url.PathEscape(args[i]), 1)
	}
	u, err := url.Parse(s.baseURL + path)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	var missing []string
	for _, option := range o.query {
		switch {
		case option.changed():
			query.Set(option.Field, option.value.String())
		case option.Required:
			missing = append(missing, "--"+option.Name)
		}
	}
	if len(missing) > 0 {
		return nil, usagef("missing required flags: %s", strings.Join(missing, ", "))
	}
	u.RawQuery = query.Encode()

	req := &request{method: o.method.HTTPMethod, url: u, header: http.Header{}}
	req.header.Set("Alguna-Version", s.apiVersion)
	req.header.Set("User-Agent", "alguna-cli/"+cliVersion())
	if o.method.HasRequestBody {
		if req.body, err = o.requestBody(); err != nil {
			return nil, err
		}
		req.header.Set("Content-Type", "application/json")
	}
	switch {
	case o.idempotencyKey != "":
		req.header.Set(idempotencyKeyHeader, o.idempotencyKey)
	case req.method != http.MethodGet && !o.method.IdentifiedByBody:
		req.header.Set(idempotencyKeyHeader, newIdempotencyKey())
	}
	return req, nil
}

// newIdempotencyKey returns a random UUID v4, the key format the SDKs generate.
func newIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// requestBody merges the field flags over --body and checks that every required
// property is set, by its flag or in --body.
func (o *methodOptions) requestBody() ([]byte, error) {
	body := map[string]any{}
	if o.bodyJSON != "" {
		raw, err := readBodyArg(o.bodyJSON, o.env)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			return nil, usagef("--body must be a JSON object: %v", err)
		}
	}
	flagNames := map[string]string{}
	for _, option := range o.body {
		if option.changed() {
			body[option.Field] = option.value.jsonValue()
		}
		flagNames[option.Field] = option.Name
	}
	var missing []string
	for _, field := range o.method.RequestBodyRequired {
		if _, set := body[field]; set {
			continue
		}
		if flag, ok := flagNames[field]; ok {
			missing = append(missing, fmt.Sprintf("--%s (or %q in --body)", flag, field))
		} else {
			missing = append(missing, fmt.Sprintf("%q in --body", field))
		}
	}
	if len(missing) > 0 {
		return nil, usagef("missing required fields: %s", strings.Join(missing, ", "))
	}
	var out bytes.Buffer
	if err := encodeJSON(&out, body, ""); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(out.Bytes()), nil
}

func readBodyArg(arg string, env *Env) ([]byte, error) {
	switch {
	case arg == "-":
		return io.ReadAll(env.Stdin)
	case strings.HasPrefix(arg, "@"):
		return os.ReadFile(strings.TrimPrefix(arg, "@"))
	default:
		return []byte(arg), nil
	}
}

// confirm asks "Void invoice inv_1? <the method's warning> [y/N]" on a terminal,
// and needs --yes otherwise.
func confirm(env *Env, method Method, args []string) error {
	words := slices.DeleteFunc(strings.Fields(method.Summary), func(w string) bool { return w == "a" || w == "an" || w == "the" })
	question := strings.Join(append(words, args...), " ") + "?"
	if !env.StdinIsTerminal {
		return usagef("%s %s Pass --yes to confirm (stdin is not a terminal).", question, method.RequireConfirmation)
	}
	fmt.Fprintf(env.Stderr, "%s %s [y/N] ", question, method.RequireConfirmation)
	answer, err := env.readLine()
	if err != nil {
		return err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return nil
	default:
		return errors.New("cancelled")
	}
}
