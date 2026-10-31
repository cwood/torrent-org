// Package desktop integrates with the user's desktop session on the machine
// running the service.
package desktop

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/cli/browser"
	"github.com/godbus/dbus/v5"
)

// Reveal shows path in the user's file manager, selected where the file
// manager supports it. A path not on disk yet, such as a download that hasn't
// started, reveals its nearest existing parent instead.
func Reveal(ctx context.Context, path string) error {
	path, err := existing(path)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return exec.CommandContext(ctx, "open", "-R", path).Run()
	}
	err = showItems(ctx, path)
	if err == nil {
		return nil
	}
	// Not every file manager implements FileManager1, but any can open the
	// containing directory.
	if openErr := browser.OpenFile(filepath.Dir(path)); openErr != nil {
		return errors.Join(err, openErr)
	}
	return nil
}

// showItems asks the freedesktop.org file manager service to open a window
// with path selected.
func showItems(ctx context.Context, path string) error {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	u := url.URL{Scheme: "file", Path: path}
	return conn.Object("org.freedesktop.FileManager1", "/org/freedesktop/FileManager1").
		CallWithContext(ctx, "org.freedesktop.FileManager1.ShowItems", 0, []string{u.String()}, "").Err
}

// existing is path or its nearest ancestor that exists.
func existing(path string) (string, error) {
	for {
		_, err := os.Stat(path)
		if err == nil {
			return path, nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", err
		}
		path = parent
	}
}
