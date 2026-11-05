package web

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cwood/torrent-org/internal/torrent"
)

type alias struct {
	Host string
	Name string
}

// trackerAliases lists the saved aliases and, from statuses, the hosts seen on
// current torrents that have none yet.
func (s *Server) trackerAliases(ctx context.Context, statuses []torrent.Status) (aliases []alias, unaliased []string, err error) {
	trackers, err := s.store.Trackers(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, host := range slices.Sorted(maps.Keys(trackers)) {
		aliases = append(aliases, alias{Host: host, Name: trackers[host]})
	}
	seen := make(map[string]bool)
	for _, st := range statuses {
		for _, h := range st.TrackerHosts() {
			if _, ok := trackers[h]; !ok {
				seen[h] = true
			}
		}
	}
	return aliases, slices.Sorted(maps.Keys(seen)), nil
}

func (s *Server) setTracker(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(r.FormValue("host")))
	name := strings.TrimSpace(r.FormValue("name"))

	var msg string
	switch {
	case host == "":
		msg = "Host is required."
	case name == "" || name == "." || name == "..":
		msg = "Directory name is required."
	case strings.Contains(name, "/"):
		msg = "Directory name cannot contain /."
	}
	if msg != "" {
		s.notice(w, http.StatusUnprocessableEntity, "Trackers", msg)
		return
	}

	if err := s.store.SetTracker(r.Context(), host, name); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/")
}

func (s *Server) deleteTracker(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteTracker(r.Context(), r.FormValue("host")); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/")
}
