// Package web serves the htmx UI: the torrent list, the rules and tracker
// editors and the add flow. Every asset is embedded in the binary.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

//go:embed templates static
var assets embed.FS

// Server holds the UI's dependencies.
type Server struct {
	client   torrent.Client
	store    *store.Store
	intake   *intake.Reconciler
	root     string
	interval time.Duration
	pages    map[string]*template.Template
	uploads  *uploads
	reveal   func(ctx context.Context, path string) error
}

// New creates a Server. interval is how often the torrent list refreshes.
//
// reveal shows a path in the user's file manager; it only makes sense when
// the browser runs on the same machine as the service. clientUI is the
// torrent client's own web interface, linked from the header when non-empty.
func New(client torrent.Client, st *store.Store, in *intake.Reconciler, root string, interval time.Duration, reveal func(ctx context.Context, path string) error, clientUI string) (*Server, error) {
	pages, err := parseTemplates(clientUI)
	if err != nil {
		return nil, err
	}
	return &Server{
		client:   client,
		store:    st,
		intake:   in,
		root:     path.Clean(root),
		interval: interval,
		pages:    pages,
		uploads:  newUploads(),
		reveal:   reveal,
	}, nil
}

var funcs = template.FuncMap{
	"percent": func(f float64) int { return int(f * 100) },
	"rate": func(b int64) string {
		if b == 0 {
			return ""
		}
		return humanize.Bytes(uint64(b)) + "/s"
	},
	"bytes":     func(b int64) string { return humanize.Bytes(uint64(b)) },
	"remaining": remaining,
}

// remaining formats a time estimate to its two largest units, which is as
// precise as a download estimate deserves.
func remaining(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// parseTemplates builds one template set per page: the shared layout and
// partials plus the page's own file, so each page can define "content".
// parseTemplates parses every page. clientUI is the torrent client's own web
// interface, linked from the header when non-empty.
func parseTemplates(clientUI string) (map[string]*template.Template, error) {
	fm := maps.Clone(funcs)
	fm["clientUI"] = func() string { return clientUI }
	base, err := template.New("").Funcs(fm).ParseFS(assets, "templates/layout.html", "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := make(map[string]*template.Template, len(files))
	for _, f := range files {
		name := strings.TrimSuffix(path.Base(f), ".html")
		if name == "layout" {
			continue
		}
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if t, err = t.ParseFS(assets, f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		pages[name] = t
	}
	return pages, nil
}

// Handler returns the UI's routes. State-changing requests from other
// origins are rejected, since any site the user visits could otherwise post
// to a loopback address.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	// The service worker and manifest must be served from the root so their
	// scope covers the whole app.
	mux.HandleFunc("GET /sw.js", serveFile(static, "sw.js"))
	mux.HandleFunc("GET /manifest.webmanifest", serveFile(static, "manifest.webmanifest"))

	mux.HandleFunc("GET /{$}", s.torrents)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /torrents/{hash}", s.detail)
	mux.HandleFunc("POST /torrents/{hash}/choose", s.chooseTorrent)
	mux.HandleFunc("POST /torrents/{hash}", s.relocate)
	mux.HandleFunc("POST /torrents/{hash}/reveal", s.revealTorrent)

	mux.HandleFunc("GET /rules", s.rules)
	mux.HandleFunc("GET /rules/new", s.newRule)
	mux.HandleFunc("GET /rules/condition", s.condition)
	mux.HandleFunc("GET /rules/group", s.group)
	mux.HandleFunc("POST /rules/preview", s.previewRule)
	mux.HandleFunc("POST /rules/new", s.saveRule)
	mux.HandleFunc("GET /rules/{id}", s.editRule)
	mux.HandleFunc("POST /rules/{id}", s.saveRule)
	mux.HandleFunc("POST /rules/{id}/delete", s.deleteRule)

	mux.HandleFunc("GET /trackers", s.trackers)
	mux.HandleFunc("POST /trackers", s.setTracker)
	mux.HandleFunc("POST /trackers/delete", s.deleteTracker)

	mux.HandleFunc("GET /add", s.addForm)
	mux.HandleFunc("POST /add/preview", s.previewAdd)
	mux.HandleFunc("POST /add/choose", s.choose)
	mux.HandleFunc("POST /add", s.add)
	mux.HandleFunc("GET /add/{id}", s.previewUpload)
	mux.HandleFunc("POST /api/uploads", s.createUpload)

	return http.NewCrossOriginProtection().Handler(mux)
}

func serveFile(fsys fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, fsys, name)
	}
}

// render executes a template into a buffer first so a failure produces a
// clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, status int, page, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, fmt.Errorf("render %s/%s: %w", page, name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) page(w http.ResponseWriter, status int, page string, data any) {
	s.render(w, status, page, "layout", data)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// chrome is the layout data every page carries.
type chrome struct {
	Title string
	Nav   string
}

// redirect sends the browser to url after a successful POST.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	http.Redirect(w, r, url, http.StatusSeeOther)
}
