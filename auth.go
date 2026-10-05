package cli

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

// checkAPIKey rejects a key that holds whitespace or control characters. Such a
// key cannot be valid, and sent in a header it fails with a confusing error.
// The message names the source and never shows the key.
func checkAPIKey(key, source string) error {
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("the API key from %s contains whitespace or control characters: check that it was copied whole", source)
		}
	}
	return nil
}

func loginCommand(env *Env) *cobra.Command {
	var apiKey string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save an API key in the config file",
		Long: "Save an API key in the config file (<user config dir>/alguna/config.json, mode 0600).\n" +
			"With --api-key the key is saved without a prompt. Otherwise, on a terminal the key is not\n" +
			"shown while you type, and with piped input the first line of stdin is read.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := strings.TrimSpace(apiKey)
			source := "--api-key"
			if !cmd.Flags().Changed("api-key") {
				source = "the prompt"
				fmt.Fprint(env.Stderr, "API key: ")
				var err error
				key, err = readSecret(env)
				fmt.Fprintln(env.Stderr)
				if err != nil {
					return fmt.Errorf("read the API key: %w", err)
				}
			}
			if key == "" {
				return errors.New("the API key is empty")
			}
			if err := checkAPIKey(key, source); err != nil {
				return err
			}
			path, err := saveConfig(fileConfig{APIKey: key})
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Stderr, "Saved the API key to %s\n", path)
			if getenv(envAPIKey) != "" {
				fmt.Fprintf(env.Stderr, "Note: $%s is set and takes precedence over the saved key.\n", envAPIKey)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiKey, "api-key", "", "API key to save, without a prompt")
	return cmd
}

func readSecret(env *Env) (string, error) {
	if env.StdinIsTerminal {
		key, err := env.ReadPassword()
		return strings.TrimSpace(string(key)), err
	}
	return env.readLine()
}

func logoutCommand(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved API key from the config file",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			path, err := saveConfig(fileConfig{})
			if err != nil {
				return err
			}
			if cfg.APIKey == "" {
				fmt.Fprintf(env.Stderr, "No saved API key in %s: nothing to remove.\n", path)
			} else {
				fmt.Fprintf(env.Stderr, "Removed the saved API key from %s\n", path)
			}
			if getenv(envAPIKey) != "" {
				fmt.Fprintf(env.Stderr, "Warning: $%s is still set, so the CLI still has an API key.\n", envAPIKey)
			}
			return nil
		},
	}
}
