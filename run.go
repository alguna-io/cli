package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Exit codes beyond 0 and 1: scripts can tell a request the API refused (4) from
// one that failed on the server or on the network (5).
const (
	exitClientError = 4
	exitServerError = 5
)

const flagJSONErrors = "json-errors"

// suggestionDistance is the most edits between a mistyped command and a name the
// error suggests. cobra's SuggestionsFor has no default when called directly.
const suggestionDistance = 2

// Env is the process environment the CLI runs in. Tests replace it.
type Env struct {
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	StdinIsTerminal  bool
	StdoutIsTerminal bool
	StderrIsTerminal bool
	// ReadPassword reads one line from the terminal without echo.
	ReadPassword func() ([]byte, error)
	HTTPClient   *http.Client
	// Sleep waits between retries.
	Sleep func(time.Duration)
}

func systemEnv() Env {
	return Env{
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		StdinIsTerminal:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutIsTerminal: term.IsTerminal(int(os.Stdout.Fd())),
		StderrIsTerminal: term.IsTerminal(int(os.Stderr.Fd())),
		ReadPassword:     func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) },
		HTTPClient:       http.DefaultClient,
		Sleep:            time.Sleep,
	}
}

// readLine reads the first line of stdin, without surrounding whitespace. Input
// that ends without a newline is one line.
func (e *Env) readLine() (string, error) {
	line, err := bufio.NewReader(e.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Main runs the CLI with a command manifest (the JSON that cmd/alguna embeds) and
// the process arguments, and exits.
func Main(manifest []byte) {
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		panic(err)
	}
	os.Exit(Run(m, systemEnv(), os.Args[1:]))
}

// Run runs the CLI and returns the exit code. Errors go to Stderr: one line on
// a terminal, JSON otherwise or with --json-errors.
func Run(m Manifest, env Env, args []string) int {
	root := newRootCommand(m, &env)
	root.SetArgs(args)
	root.SetIn(env.Stdin)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	// The flag is not parsed when the command line is wrong, so look for it too.
	forceJSON, _ := root.PersistentFlags().GetBool(flagJSONErrors)
	forceJSON = forceJSON || slices.Contains(args, "--"+flagJSONErrors)
	return writeError(env.Stderr, err, cmd.CommandPath(), !forceJSON && env.StderrIsTerminal)
}

// epilogueTemplate prints a method's epilogue (an x-forge-epilogue) after its help.
const epilogueTemplate = `{{with index .Annotations "epilogue"}}
{{.}}
{{end}}`

func newRootCommand(m Manifest, env *Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "alguna",
		Short: "Command-line client for the Alguna API",
		Long: "Command-line client for the Alguna API.\n\n" +
			"Every API operation is a command: alguna <resource> <operation>.\n" +
			"Responses go to stdout as JSON. Errors go to stderr: one line on a terminal, JSON\n" +
			"otherwise or with --json-errors. The exit code is 4 for a 4xx response, 5 for a 5xx\n" +
			"response or a network error, and 1 for other errors.",
		SilenceErrors:              true,
		SilenceUsage:               true,
		Args:                       subcommandArgs,
		SuggestionsMinimumDistance: suggestionDistance,
		RunE:                       showHelp,
	}
	root.PersistentFlags().Bool(flagJSONErrors, false, "Print errors as JSON, even on a terminal")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{msg: err.Error()}
	})
	root.SetHelpTemplate(root.HelpTemplate() + epilogueTemplate)

	root.AddGroup(&cobra.Group{ID: "api", Title: "API commands:"}, &cobra.Group{ID: "cli", Title: "CLI commands:"})
	root.SetHelpCommandGroupID("cli")
	root.SetCompletionCommandGroupID("cli")
	for _, command := range m.Commands {
		group := &cobra.Command{
			Use:                        command.Name,
			Short:                      command.Description,
			GroupID:                    "api",
			Args:                       subcommandArgs,
			SuggestionsMinimumDistance: suggestionDistance,
			RunE:                       showHelp,
		}
		for _, method := range command.Methods {
			group.AddCommand(methodCommand(m, method, env))
		}
		root.AddCommand(group)
	}
	for _, cmd := range []*cobra.Command{loginCommand(env), logoutCommand(env), versionCommand(m, env)} {
		cmd.GroupID = "cli"
		root.AddCommand(cmd)
	}
	return root
}

func showHelp(cmd *cobra.Command, _ []string) error { return cmd.Help() }

// noArgs rejects any argument, naming the first.
func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q", args[0])
	}
	return nil
}

// subcommandArgs is the argument check of a command that only groups
// subcommands: no arguments shows its help, and an unknown subcommand is an
// error that suggests close names.
func subcommandArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		for i, s := range suggestions {
			suggestions[i] = strconv.Quote(s)
		}
		msg += ". Did you mean " + strings.Join(suggestions, " or ") + "?"
	}
	return &usageError{msg: msg}
}

func cliVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

func versionCommand(m Manifest, env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the default API version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(env.Stdout, "alguna %s\ndefault API version %s\n", cliVersion(), m.APIVersion)
			return err
		},
	}
}
