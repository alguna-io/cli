package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	envAPIKey     = "ALGUNA_API_KEY"
	envBaseURL    = "ALGUNA_BASE_URL"
	envAPIVersion = "ALGUNA_API_VERSION"
)

// fileConfig is the config file that `alguna login` writes.
type fileConfig struct {
	APIKey string `json:"api_key,omitempty"`
}

// getenv returns the variable without surrounding whitespace, so a value with a
// trailing newline (from a file or a secret manager) works, and a blank one is unset.
func getenv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w", err)
	}
	return filepath.Join(dir, "alguna", "config.json"), nil
}

// loadConfig returns an empty config when the file does not exist.
func loadConfig() (fileConfig, error) {
	var cfg fileConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// saveConfig writes the file with mode 0600, because it holds the API key.
// An empty config removes the file.
func saveConfig(cfg fileConfig) (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	if cfg == (fileConfig{}) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("remove %s: %w", path, err)
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	// os.WriteFile keeps the mode of a file that already exists, so set it again.
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	return path, nil
}

// settings are the resolved connection settings for one command run.
type settings struct {
	apiKey string
	// baseURL is an http or https URL without a trailing slash.
	baseURL    string
	apiVersion string
}

// resolveSettings applies the precedence flag > environment > config file > manifest
// default. A key that holds whitespace or control characters is an error, and so
// is a base URL that is not http or https.
func resolveSettings(m Manifest, flags apiFlags) (settings, error) {
	key, source := strings.TrimSpace(flags.apiKey), "--api-key"
	if key == "" {
		key, source = getenv(envAPIKey), "$"+envAPIKey
	}
	if key == "" {
		cfg, err := loadConfig()
		if err != nil {
			return settings{}, err
		}
		key, source = strings.TrimSpace(cfg.APIKey), "the config file"
	}
	if err := checkAPIKey(key, source); err != nil {
		return settings{}, err
	}
	baseURL := cmp.Or(strings.TrimSpace(flags.baseURL), getenv(envBaseURL), m.BaseURL)
	if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return settings{}, fmt.Errorf("base URL %q is not an http or https URL", baseURL)
	}
	return settings{
		apiKey:     key,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiVersion: cmp.Or(strings.TrimSpace(flags.apiVersion), getenv(envAPIVersion), m.APIVersion),
	}, nil
}
