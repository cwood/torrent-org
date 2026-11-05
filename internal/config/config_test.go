package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv(PasswordEnv, "")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.NoError(t, err)
	require.Equal(t, Config{
		Listen:          "127.0.0.1:9092",
		Database:        "/xdg/data/torrent-org/torrent-org.db",
		Root:            "/home/test/torrents",
		PollInterval:    3 * time.Second,
		PrivateShortcut: "ctrl+shift+h",
		Transmission:    Transmission{URL: "http://localhost:9091/transmission/rpc"},
	}, cfg)
	require.NoError(t, cfg.Validate())
}

func TestLoadFile(t *testing.T) {
	t.Setenv("HOME", "/home/test")
	t.Setenv(PasswordEnv, "from-env")

	cfg, err := Load(writeConfig(t, `
listen: 0.0.0.0:8080
database: ~/torrents.db
root: ~/media/torrents
poll_interval: 10s
private_shortcut: Ctrl+Alt+K
transmission:
  url: https://seedbox.example/transmission/rpc
  username: me
  password: from-file
`))
	require.NoError(t, err)
	require.Equal(t, Config{
		Listen:          "0.0.0.0:8080",
		Database:        "/home/test/torrents.db",
		Root:            "/home/test/media/torrents",
		PollInterval:    10 * time.Second,
		PrivateShortcut: "ctrl+alt+k",
		Transmission:    Transmission{URL: "https://seedbox.example/transmission/rpc", Username: "me", Password: "from-env"},
	}, cfg)
	require.NoError(t, cfg.Validate())
}

func TestLoadInvalidYAML(t *testing.T) {
	_, err := Load(writeConfig(t, "listen: [unterminated"))
	require.Error(t, err)
}

func TestValidate(t *testing.T) {
	valid := Config{
		Root:            "/data/torrents",
		PollInterval:    time.Second,
		PrivateShortcut: "ctrl+shift+h",
		Transmission:    Transmission{URL: "http://localhost:9091/transmission/rpc"},
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "missing root", mutate: func(c *Config) { c.Root = "" }, wantErr: "root is required"},
		{name: "relative root", mutate: func(c *Config) { c.Root = "torrents" }, wantErr: "absolute path"},
		{name: "zero poll interval", mutate: func(c *Config) { c.PollInterval = 0 }, wantErr: "poll_interval"},
		{name: "digit shortcut", mutate: func(c *Config) { c.PrivateShortcut = "meta+1" }},
		{name: "shortcut without modifier", mutate: func(c *Config) { c.PrivateShortcut = "h" }, wantErr: "private_shortcut"},
		{name: "shift only shortcut", mutate: func(c *Config) { c.PrivateShortcut = "shift+h" }, wantErr: "private_shortcut"},
		{name: "repeated modifier", mutate: func(c *Config) { c.PrivateShortcut = "ctrl+ctrl+h" }, wantErr: "private_shortcut"},
		{name: "unknown modifier", mutate: func(c *Config) { c.PrivateShortcut = "hyper+h" }, wantErr: "private_shortcut"},
		{name: "named key shortcut", mutate: func(c *Config) { c.PrivateShortcut = "ctrl+enter" }, wantErr: "private_shortcut"},
		{name: "empty shortcut", mutate: func(c *Config) { c.PrivateShortcut = "" }, wantErr: "private_shortcut"},
		{name: "bad url", mutate: func(c *Config) { c.Transmission.URL = "localhost:9091" }, wantErr: "transmission.url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
