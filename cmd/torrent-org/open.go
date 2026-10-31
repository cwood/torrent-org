package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cli/browser"

	"github.com/cwood/torrent-org/internal/config"
)

// openCmd opens the running service's UI, or the preview for file when one
// is given.
func openCmd(ctx context.Context, cfg config.Config, file string) error {
	base, err := serviceURL(cfg.Listen)
	if err != nil {
		return err
	}
	if file == "" {
		return openUI(ctx, base, browser.OpenURL)
	}
	return open(ctx, base, file, browser.OpenURL)
}

// openUI launches the UI after checking the service is up, so a launcher
// click reports a stopped service instead of opening a dead page.
func openUI(ctx context.Context, base string, launch func(string) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/manifest.webmanifest", nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("is torrent-org running at %s? %w", base, err)
	}
	_ = res.Body.Close()
	return launch(base + "/")
}

// notify shows err as a desktop notification. open usually runs from a file
// manager or launcher where stderr goes nowhere. Best effort, Linux only.
func notify(err error) {
	if runtime.GOOS != "linux" {
		return
	}
	_ = exec.Command("notify-send", "--app-name=torrent-org", "--icon=torrent-org", "torrent-org", err.Error()).Run()
}

// open hands a .torrent file to the running service and launches its preview
// page, which shows the rule it matched and lets the user pick files.
func open(ctx context.Context, base, file string, launch func(string) error) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/uploads", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-bittorrent")

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("is torrent-org running at %s? %w", base, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("upload %s: %s", file, strings.TrimSpace(string(msg)))
	}

	var created struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		return fmt.Errorf("decode upload response: %w", err)
	}
	return launch(base + created.URL)
}

// serviceURL is the base URL for reaching a service listening on addr. A
// wildcard listen address is reached over loopback.
func serviceURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	if ip, err := netip.ParseAddr(host); host == "" || (err == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}
