package web

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/torrent"
)

type torrentPage struct {
	chrome
	Status torrent.Status
	// Rule and Manual describe the current assignment.
	Rule   *rules.Rule
	Manual bool
	// Auto is where the rules would place the torrent today.
	Auto rules.Placement
	// Placement is where the selected choice would place it.
	Placement rules.Placement
	Picker    picker
	// Files are the torrent's files, checked when selected for download.
	Files fileChoices
	// Unchanged is true when saving would neither move, regroup nor change
	// whether the assignment is manual.
	Unchanged bool
	Error     string
}

// detail shows a torrent and lets the user re-place it.
func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	s.showTorrent(w, r, http.StatusOK, "", "")
}

// chooseTorrent re-renders the torrent page when the rule picker changes.
func (s *Server) chooseTorrent(w http.ResponseWriter, r *http.Request) {
	s.showTorrent(w, r, http.StatusOK, r.FormValue("rule"), "")
}

// relocate moves the torrent to the chosen placement, pausing it while its
// files move.
func (s *Server) relocate(w http.ResponseWriter, r *http.Request) {
	hash, choice := r.PathValue("hash"), r.FormValue("rule")
	st, ok, err := s.status(r, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	rs, trackers, err := s.ruleset(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	p, err := s.placeChoice(st.Torrent, choice, rs, trackers)
	if err != nil {
		s.showTorrent(w, r, http.StatusUnprocessableEntity, choiceAuto, err.Error())
		return
	}
	err = s.intake.Relocate(r.Context(), []intake.Move{{Hash: hash, Placement: p, Manual: choice != choiceAuto}})
	if err != nil {
		s.showTorrent(w, r, http.StatusBadGateway, choice, err.Error())
		return
	}
	redirect(w, r, "/")
}

// chooseFiles changes which of the torrent's files are downloaded. Files
// unchecked after downloading keep their data; Transmission only stops
// fetching them.
func (s *Server) chooseFiles(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash := r.PathValue("hash")
	st, ok, err := s.status(r, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	selected := selectedFiles(r)
	var wanted, unwanted []int
	for _, f := range st.Files {
		switch {
		case selected[f.Index] && !f.Wanted:
			wanted = append(wanted, f.Index)
		case !selected[f.Index] && f.Wanted:
			unwanted = append(unwanted, f.Index)
		}
	}
	if !slices.ContainsFunc(st.Files, func(f torrent.File) bool { return selected[f.Index] }) {
		s.showTorrent(w, r, http.StatusUnprocessableEntity, "", "Select at least one file to download.")
		return
	}
	if len(wanted)+len(unwanted) > 0 {
		if err := s.client.SetWanted(r.Context(), hash, wanted, unwanted); err != nil {
			s.showTorrent(w, r, http.StatusBadGateway, "", "Cannot change files: "+err.Error())
			return
		}
	}
	redirect(w, r, "/torrents/"+hash)
}

// revealTorrent shows the torrent's files in the desktop file manager. The
// path comes from the client, never the request, so a page can't make the
// service open arbitrary locations.
func (s *Server) revealTorrent(w http.ResponseWriter, r *http.Request) {
	st, ok, err := s.status(r, r.PathValue("hash"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.reveal(r.Context(), st.DataPath()); err != nil {
		s.fail(w, fmt.Errorf("open file manager: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// status finds the torrent with hash in the client.
func (s *Server) status(r *http.Request, hash string) (torrent.Status, bool, error) {
	statuses, err := s.client.Torrents(r.Context())
	if err != nil {
		return torrent.Status{}, false, err
	}
	i := slices.IndexFunc(statuses, func(st torrent.Status) bool { return st.Hash == hash })
	if i < 0 {
		return torrent.Status{}, false, nil
	}
	return statuses[i], true, nil
}

// showTorrent renders the torrent page with choice selected; an empty choice
// selects whatever reproduces the current assignment.
func (s *Server) showTorrent(w http.ResponseWriter, r *http.Request, code int, choice, errMsg string) {
	hash := r.PathValue("hash")
	st, ok, err := s.status(r, hash)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	assignments, err := s.store.Assignments(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	rs, trackers, err := s.ruleset(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	a := assignments[hash]
	current := choiceOf(a)
	if choice == "" {
		choice = current
	}
	p, err := s.placeChoice(st.Torrent, choice, rs, trackers)
	if err != nil {
		// The assigned rule was deleted; fall back to the rules' pick.
		choice = choiceAuto
		p = rules.Place(st.Torrent, rs, trackers, s.root)
	}

	page := torrentPage{
		chrome:    chrome{st.Name, "torrents"},
		Status:    st,
		Manual:    a.Manual,
		Auto:      rules.Place(st.Torrent, rs, trackers, s.root),
		Placement: p,
		Picker:    picker{Rules: rs, Choice: choice, Action: "/torrents/" + hash + "/choose", Target: "torrent"},
		Unchanged: p.Dir == st.Dir && p.Group == st.Group && choice == current,
		Error:     errMsg,
	}
	if i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.ID == a.RuleID }); i >= 0 {
		page.Rule = &rs[i]
	}
	for _, f := range st.Files {
		page.Files = append(page.Files, fileChoice{File: f, Checked: f.Wanted})
	}
	s.page(w, code, "torrent", page)
}
