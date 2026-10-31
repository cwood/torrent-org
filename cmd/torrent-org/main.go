// Command torrent-org organizes Transmission downloads into
// <root>/<tracker>/<rule> directories using rules managed from a web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cwood/torrent-org/internal/config"
	"github.com/cwood/torrent-org/internal/desktop"
	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/transmission"
	"github.com/cwood/torrent-org/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("torrent-org", flag.ContinueOnError)
	configPath := fs.String("config", defaultPath, "path to config file")
	fs.String("listen", "", "HTTP listen address")
	fs.String("database", "", "path to SQLite database")
	fs.String("root", "", "base download directory as Transmission sees it (default ~/torrents)")
	fs.String("transmission-url", "", "Transmission RPC URL")
	fs.String("transmission-username", "", "Transmission RPC username (password via "+config.PasswordEnv+")")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage:\n  torrent-org [flags]                 run the service\n  torrent-org [flags] open [FILE]     open the UI, or preview FILE in it\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// Only flags set explicitly override the file, so an empty default never
	// clobbers a configured value.
	overrides := map[string]*string{
		"listen":                &cfg.Listen,
		"database":              &cfg.Database,
		"root":                  &cfg.Root,
		"transmission-url":      &cfg.Transmission.URL,
		"transmission-username": &cfg.Transmission.Username,
	}
	fs.Visit(func(f *flag.Flag) {
		if dst, ok := overrides[f.Name]; ok {
			*dst = f.Value.String()
		}
	})

	switch fs.Arg(0) {
	case "":
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config %s: %w", *configPath, err)
		}
		return serve(ctx, cfg)
	case "open":
		if fs.NArg() > 2 {
			fs.Usage()
			return errors.New("open takes at most one .torrent file")
		}
		err := openCmd(ctx, cfg, fs.Arg(1))
		if err != nil {
			notify(err)
		}
		return err
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", fs.Arg(0))
}

// serve runs the reconciler and the web UI until ctx is done.
func serve(ctx context.Context, cfg config.Config) error {

	st, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			slog.Warn("close database", "err", err)
		}
	}()

	slog.Info("database ready", "path", cfg.Database)

	client, err := transmission.New(cfg.Transmission)
	if err != nil {
		return err
	}
	// Transmission may be a desktop app that starts after this service, so
	// only an incompatible server is fatal; passes retry until it is up.
	switch err := client.Check(ctx); {
	case errors.Is(err, transmission.ErrIncompatible):
		return err
	case err != nil:
		slog.Warn("transmission unreachable; will keep retrying", "url", cfg.Transmission.URL, "err", err)
	default:
		slog.Info("transmission ready", "url", cfg.Transmission.URL, "root", cfg.Root)
	}

	rec := intake.NewReconciler(client, st, cfg.Root)
	ui, err := web.New(client, st, rec, cfg.Root, cfg.PollInterval, desktop.Reveal)
	if err != nil {
		return err
	}

	// Bind before starting the reconciler so a second instance fails here
	// instead of running a pass against the same database first.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	srv := &http.Server{
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Shutdown does not cancel in-flight requests; deriving them from ctx
		// ends long-lived SSE streams as soon as a signal arrives.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	g.Go(func() error {
		rec.Run(ctx, cfg.PollInterval)
		return nil
	})
	g.Go(func() error {
		slog.Info("serving ui", "url", "http://"+cfg.Listen)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		// ctx is already cancelled, so Shutdown needs its own deadline.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}
