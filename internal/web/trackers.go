package web

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

type alias struct {
	Host string
	Name string
}

type trackersPage struct {
	chrome
	Aliases []alias
	// Unaliased are hosts seen on current torrents with no alias yet.
	Unaliased []string
	Error     string
}

func (s *Server) trackersPage(r *http.Request, errMsg string) (trackersPage, error) {
	trackers, err := s.store.Trackers(r.Context())
	if err != nil {
		return trackersPage{}, err
	}
	p := trackersPage{chrome: chrome{"Trackers", "trackers"}, Error: errMsg}
	for _, host := range slices.Sorted(maps.Keys(trackers)) {
		p.Aliases = append(p.Aliases, alias{Host: host, Name: trackers[host]})
	}

	// Seen hosts are a convenience; an unreachable client shouldn't block
	// editing aliases.
	statuses, _ := s.client.Torrents(r.Context())
	seen := make(map[string]bool)
	for _, st := range statuses {
		for _, h := range st.TrackerHosts() {
			if _, ok := trackers[h]; !ok {
				seen[h] = true
			}
		}
	}
	p.Unaliased = slices.Sorted(maps.Keys(seen))
	return p, nil
}

func (s *Server) trackers(w http.ResponseWriter, r *http.Request) {
	p, err := s.trackersPage(r, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.page(w, http.StatusOK, "trackers", p)
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
		p, err := s.trackersPage(r, msg)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.page(w, http.StatusUnprocessableEntity, "trackers", p)
		return
	}

	if err := s.store.SetTracker(r.Context(), host, name); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/trackers")
}

func (s *Server) deleteTracker(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteTracker(r.Context(), r.FormValue("host")); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/trackers")
}
