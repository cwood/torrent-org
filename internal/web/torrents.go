package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/torrent"
)

// row is a torrent as shown in the list.
type row struct {
	torrent.Status
	Rule   *rules.Rule
	Manual bool
}

// Totals summarize a set of rows for a header.
type Totals struct {
	Count int
	// Rate is the total download speed while downloading, or upload speed
	// once completed.
	Rate int64
	// ETA is the longest estimate among the rows, so the whole set is done
	// by then; zero when nothing has an estimate.
	ETA    time.Duration
	Errors int
	// Up marks Rate as upload speed.
	Up bool
}

func (t *Totals) add(r row) {
	rate := r.RateDown
	if t.Up {
		rate = r.RateUp
	}
	t.Count++
	t.Rate += rate
	t.ETA = max(t.ETA, r.ETA)
	if r.Error != "" {
		t.Errors++
	}
}

// group is one rule's torrents within a section. Rule is nil for torrents no
// rule placed. Key identifies the group to the browser, which remembers
// whether it is collapsed.
type group struct {
	Rule     *rules.Rule
	Key      string
	Torrents []row
	Totals
}

// section is a top-level part of the list. Torrents are split by whether
// they have finished downloading rather than by their current state, so
// pausing or queueing one doesn't move it between sections.
type section struct {
	Key    string
	Title  string
	Groups []group
	Totals
	// Open is the default before the browser remembers a choice.
	Open bool
}

// sections splits rows into Downloading, always present, and Completed,
// present only when it has torrents. Within each, torrents are grouped by
// rule in priority order, with torrents no rule placed last.
func sections(rows []row, rs []rules.Rule) []section {
	var downloading, completed []row
	for _, r := range rows {
		if r.Complete() {
			completed = append(completed, r)
			continue
		}
		downloading = append(downloading, r)
	}
	out := []section{newSection("downloading", "Downloading", downloading, rs, false)}
	if len(completed) > 0 {
		out = append(out, newSection("completed", "Completed", completed, rs, true))
	}
	return out
}

// newSection groups rows by rule. Completed sections start collapsed and
// total upload speed; downloading ones start open and total download speed.
func newSection(key, title string, rows []row, rs []rules.Rule, completed bool) section {
	byRule := make(map[int64][]row)
	for _, r := range rows {
		var id int64
		if r.Rule != nil {
			id = r.Rule.ID
		}
		byRule[id] = append(byRule[id], r)
	}

	s := section{Key: key, Title: title, Open: !completed, Totals: Totals{Up: completed}}
	addGroup := func(rule *rules.Rule, key string, rows []row) {
		if len(rows) == 0 {
			return
		}
		slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.Name, b.Name) })
		g := group{Rule: rule, Key: key, Torrents: rows, Totals: Totals{Up: completed}}
		for _, r := range rows {
			g.add(r)
			s.add(r)
		}
		s.Groups = append(s.Groups, g)
	}
	for i := range rs {
		addGroup(&rs[i], "rule-"+strconv.FormatInt(rs[i].ID, 10), byRule[rs[i].ID])
	}
	addGroup(nil, "none", byRule[0])
	return s
}

type listData struct {
	Sections []section
	Empty    bool
	Error    string
}

func (s *Server) list(ctx context.Context) listData {
	statuses, err := s.client.Torrents(ctx)
	if err != nil {
		return listData{Error: fmt.Sprintf("Cannot reach the torrent client: %v", err)}
	}
	assignments, err := s.store.Assignments(ctx)
	if err != nil {
		return listData{Error: err.Error()}
	}
	rs, err := s.store.Rules(ctx)
	if err != nil {
		return listData{Error: err.Error()}
	}
	byID := make(map[int64]*rules.Rule, len(rs))
	for i := range rs {
		byID[rs[i].ID] = &rs[i]
	}

	rows := make([]row, 0, len(statuses))
	for _, st := range statuses {
		a := assignments[st.Hash]
		rows = append(rows, row{Status: st, Rule: byID[a.RuleID], Manual: a.Manual})
	}
	return listData{Sections: sections(rows, rs), Empty: len(rows) == 0}
}

func (s *Server) torrents(w http.ResponseWriter, r *http.Request) {
	s.page(w, http.StatusOK, "torrents", struct {
		chrome
		List listData
	}{chrome{"Torrents", "torrents"}, s.list(r.Context())})
}

// events streams the rendered list as server-sent events whenever it
// changes, for the htmx sse extension to swap in.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	t := time.NewTicker(s.interval)
	defer t.Stop()
	var last []byte
	for {
		var buf bytes.Buffer
		if err := s.pages["torrents"].ExecuteTemplate(&buf, "list", s.list(r.Context())); err != nil {
			s.fail(w, err)
			return
		}
		if b := bytes.TrimSpace(buf.Bytes()); !bytes.Equal(b, last) {
			last = b
			if err := writeEvent(w, "list", last); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// writeEvent writes one SSE event. Every line of a multi-line payload needs
// its own data: prefix.
func writeEvent(w http.ResponseWriter, event string, data []byte) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "event: %s\n", event)
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, err := buf.WriteTo(w)
	return err
}
