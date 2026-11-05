// Package config loads the service's infrastructure settings: where to listen,
// where the database lives and how to reach Transmission. Rules and tracker
// aliases are managed in the database, not here.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PasswordEnv, when non-empty, overrides the Transmission password so it can
// be kept out of the config file.
const PasswordEnv = "TORRENT_ORG_TRANSMISSION_PASSWORD"

// Config is the contents of config.yaml.
type Config struct {
	Listen   string `yaml:"listen"`
	Database string `yaml:"database"`
	// Root is the base download directory as Transmission sees it, which may
	// differ from this machine's filesystem when Transmission is remote. It
	// defaults to ~/torrents.
	Root         string        `yaml:"root"`
	PollInterval time.Duration `yaml:"poll_interval"`
	// PrivateShortcut is the key combination that shows or hides private
	// rules, such as "ctrl+shift+h".
	PrivateShortcut string       `yaml:"private_shortcut"`
	Transmission    Transmission `yaml:"transmission"`
}

// Transmission holds the RPC connection settings.
type Transmission struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// DefaultPath returns $XDG_CONFIG_HOME/torrent-org/config.yaml, falling back
// to ~/.config. XDG is used on macOS too rather than ~/Library so the file
// lives alongside other dotfiles.
func DefaultPath() (string, error) {
	dir, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "torrent-org", "config.yaml"), nil
}

// Load reads the config file at p over the defaults. A missing file yields
// the defaults. The result is not validated; call [Config.Validate] after
// applying any overrides.
func Load(p string) (Config, error) {
	dataDir, err := xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Listen:          "127.0.0.1:9092",
		Database:        filepath.Join(dataDir, "torrent-org", "torrent-org.db"),
		Root:            "~/torrents",
		PollInterval:    3 * time.Second,
		PrivateShortcut: "ctrl+shift+h",
		Transmission:    Transmission{URL: "http://localhost:9091/transmission/rpc"},
	}

	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", p, err)
	}

	cfg.PrivateShortcut = strings.ToLower(cfg.PrivateShortcut)
	if pw := os.Getenv(PasswordEnv); pw != "" {
		cfg.Transmission.Password = pw
	}
	for _, p := range []*string{&cfg.Database, &cfg.Root} {
		if *p, err = expandHome(*p); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// validShortcut reports whether s is distinct modifiers followed by one letter
// or digit. Shift alone isn't enough, since that would fire while typing.
func validShortcut(s string) bool {
	parts := strings.Split(s, "+")
	key, mods := parts[len(parts)-1], parts[:len(parts)-1]
	if len(key) != 1 || !strings.Contains("abcdefghijklmnopqrstuvwxyz0123456789", key) {
		return false
	}
	seen := make(map[string]bool)
	for _, m := range mods {
		if m != "ctrl" && m != "shift" && m != "alt" && m != "meta" || seen[m] {
			return false
		}
		seen[m] = true
	}
	return seen["ctrl"] || seen["alt"] || seen["meta"]
}

// Validate reports settings the service cannot run with.
func (c Config) Validate() error {
	var errs []error
	if c.Root == "" {
		errs = append(errs, errors.New("root is required"))
	} else if !path.IsAbs(c.Root) {
		errs = append(errs, fmt.Errorf("root %q must be an absolute path", c.Root))
	}
	if c.PollInterval <= 0 {
		errs = append(errs, fmt.Errorf("poll_interval %s must be positive", c.PollInterval))
	}
	if !validShortcut(c.PrivateShortcut) {
		errs = append(errs, fmt.Errorf("private_shortcut %q must be modifiers and one letter or digit, like ctrl+shift+h, with ctrl, alt or meta among them", c.PrivateShortcut))
	}
	if u, err := url.Parse(c.Transmission.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		errs = append(errs, fmt.Errorf("transmission.url %q must be an http(s) URL", c.Transmission.URL))
	}
	return errors.Join(errs...)
}

func xdgDir(env, fallback string) (string, error) {
	if dir := os.Getenv(env); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, fallback), nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
