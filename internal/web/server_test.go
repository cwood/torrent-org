package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
	"github.com/cwood/torrent-org/internal/torrent/torrentmock"
	"github.com/cwood/torrent-org/internal/torrent/torrenttest"
)

const root = "/data/torrents"

type harness struct {
	t      *testing.T
	client *torrentmock.Client
	store  *store.Store
	h      http.Handler
	// revealed records paths shown in the file manager; revealErr fails it.
	revealed  []string
	revealErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.SetTracker(t.Context(), "flacsfor.me", "red"))

	client := torrentmock.NewClient(t)
	h := &harness{t: t, client: client, store: st}
	reveal := func(_ context.Context, path string) error {
		h.revealed = append(h.revealed, path)
		return h.revealErr
	}
	srv, err := New(client, st, intake.NewReconciler(client, st, root), root, 10*time.Millisecond, reveal, "http://localhost:9091/transmission/web/", "ctrl+shift+h")
	require.NoError(t, err)
	h.h = srv.Handler()
	return h
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(httptest.NewRequest(http.MethodGet, path, nil))
}

func (h *harness) post(path string, form url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return h.do(req)
}

func (h *harness) audioRule() rules.Rule {
	h.t.Helper()
	r := rules.Rule{
		Name: "🎵 Audio", Color: "#ff8800", Path: "{root}/{tracker}/audio",
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}}},
	}
	var err error
	r.ID, err = h.store.SaveRule(h.t.Context(), r)
	require.NoError(h.t, err)
	return r
}

func (h *harness) rules() []rules.Rule {
	h.t.Helper()
	rs, err := h.store.Rules(h.t.Context())
	require.NoError(h.t, err)
	return rs
}

func album(hash, dir string) torrent.Status {
	return torrent.Status{
		Torrent: torrent.Torrent{
			Hash: hash, Name: "Artist - Album",
			Trackers: []string{"https://flacsfor.me/x/announce"},
			Files:    []torrent.File{{Path: "Artist - Album/01.flac", Size: 100}},
		},
		Dir: dir, Progress: 0.42, RateDown: 2048, State: torrent.StateDownloading,
	}
}

func TestSections(t *testing.T) {
	audio := &rules.Rule{ID: 1, Name: "🎵 Audio"}
	video := &rules.Rule{ID: 2, Name: "🎬 Video"}
	rs := []rules.Rule{*video, *audio}
	at := func(name string, rule *rules.Rule, progress float64, state torrent.State, down, up int64, eta time.Duration, errMsg string) row {
		return row{Rule: rule, Status: torrent.Status{
			Torrent: torrent.Torrent{Name: name}, Dir: root, Progress: progress,
			State: state, RateDown: down, RateUp: up, ETA: eta, Error: errMsg,
		}}
	}
	ss := sections([]row{
		at("b-album", audio, 0.5, torrent.StateDownloading, 100, 7, 2*time.Minute, ""),
		at("a-album", audio, 0.3, torrent.StateStopped, 0, 0, 0, ""),
		at("movie", video, 0.1, torrent.StateDownloading, 50, 0, time.Hour, ""),
		at("loose", nil, 0, torrent.StateQueued, 0, 0, 0, "tracker gone"),
		at("seeding", audio, 1, torrent.StateSeeding, 0, 50, 0, ""),
		at("finished", nil, 1, torrent.StateStopped, 0, 0, 0, "missing files"),
	}, rs)

	type summary struct {
		Key      string
		Torrents []string
		Totals
	}
	groups := func(s section) []summary {
		var out []summary
		for _, g := range s.Groups {
			sum := summary{Key: g.Key, Totals: g.Totals}
			for _, r := range g.Torrents {
				sum.Torrents = append(sum.Torrents, r.Name)
			}
			out = append(out, sum)
		}
		return out
	}

	require.Len(t, ss, 2)
	down, done := ss[0], ss[1]
	require.Equal(t, "downloading", down.Key)
	require.True(t, down.Open)
	require.Equal(t, Totals{Count: 4, Rate: 150, ETA: time.Hour, Errors: 1}, down.Totals, "download speed only; ETA is the slowest")
	require.Equal(t, []summary{
		{Key: "rule-2", Torrents: []string{"movie"}, Totals: Totals{Count: 1, Rate: 50, ETA: time.Hour}},
		{Key: "rule-1", Torrents: []string{"a-album", "b-album"}, Totals: Totals{Count: 2, Rate: 100, ETA: 2 * time.Minute}},
		{Key: "none", Torrents: []string{"loose"}, Totals: Totals{Count: 1, Errors: 1}},
	}, groups(down), "rules in the order given, paused torrents stay, no rule last")

	require.Equal(t, "completed", done.Key)
	require.False(t, done.Open)
	require.Equal(t, Totals{Count: 2, Rate: 50, Errors: 1, Up: true}, done.Totals, "completed totals upload speed")
	require.Equal(t, []summary{
		{Key: "rule-1", Torrents: []string{"seeding"}, Totals: Totals{Count: 1, Rate: 50, Up: true}},
		{Key: "none", Torrents: []string{"finished"}, Totals: Totals{Count: 1, Errors: 1, Up: true}},
	}, groups(done), "rules without torrents are hidden")

	only := sections([]row{at("x", nil, 0.5, torrent.StateDownloading, 0, 0, 0, "")}, rs)
	require.Len(t, only, 1, "an empty Completed section is hidden")
	empty := sections(nil, rs)
	require.Len(t, empty, 1, "Downloading is always shown")
	require.Empty(t, empty[0].Groups)
}

func TestRemaining(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                "45s",
		12*time.Minute + 30*time.Second: "12m",
		3*time.Hour + 5*time.Minute:     "3h 5m",
		52 * time.Hour:                  "2d 4h",
	} {
		require.Equal(t, want, remaining(d), d)
	}
}

func TestClientUILink(t *testing.T) {
	h := newHarness(t)
	require.Contains(t, h.get("/add").Body.String(), `<a class="icon" href="http://localhost:9091/transmission/web/" target="_blank" rel="noopener"`)
}

func TestNewRuleColors(t *testing.T) {
	h := newHarness(t)
	body := h.get("/rules/new").Body.String()
	require.Contains(t, body, `name="color" value="#00a6ff"`, "new rules default to the macOS new-group colour")
	for _, sw := range swatches {
		require.Contains(t, body, `data-color="`+sw.Color+`" title="`+sw.Name+`"`)
	}
}

func TestReveal(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album("aaa", root+"/red/audio")}, nil).Times(4)

	require.Contains(t, h.get("/").Body.String(), `hx-post="/torrents/aaa/reveal"`)
	require.Contains(t, h.get("/torrents/aaa").Body.String(), `hx-post="/torrents/aaa/reveal"`)

	res := h.post("/torrents/aaa/reveal", nil)
	require.Equal(t, http.StatusNoContent, res.Code)
	require.Equal(t, []string{root + "/red/audio/Artist - Album"}, h.revealed)

	h.revealErr = errors.New("no session bus")
	res = h.post("/torrents/aaa/reveal", nil)
	require.Equal(t, http.StatusInternalServerError, res.Code)
	require.Contains(t, res.Body.String(), "open file manager: no session bus")
}

func TestRevealUnknown(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
	require.Equal(t, http.StatusNotFound, h.post("/torrents/nope/reveal", nil).Code)
	require.Empty(t, h.revealed)
}

func TestTorrentsPageSections(t *testing.T) {
	h := newHarness(t)
	done := album("bbb", root+"/red/audio")
	done.Progress, done.RateDown, done.RateUp, done.State = 1, 0, 3072, torrent.StateSeeding
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{done}, nil).Once()

	body := h.get("/").Body.String()
	require.Contains(t, body, `<details class="layer section" data-section="downloading" open>`)
	require.Contains(t, body, "Nothing downloading.")
	require.Contains(t, body, `<details class="layer section" data-section="completed">`, "completed starts collapsed")
	require.Contains(t, body, `<progress class="bar-seeding done"`, "completed torrents keep a green bar, as on macOS")
	require.Contains(t, body, "↑ 3.1 kB/s")
	require.NotContains(t, body, "No torrents yet")
}

func TestTorrentsPage(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()
	require.NoError(t, h.store.Assign(t.Context(), store.Assignment{Hash: "aaa", RuleID: audio.ID, Manual: true}))
	stalled := album("bbb", root)
	stalled.Name, stalled.RateDown = "Stalled", 0
	downloading := album("aaa", root+"/red/audio")
	downloading.ETA = 90 * time.Second
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{downloading, stalled}, nil).Once()

	res := h.get("/")
	require.Equal(t, http.StatusOK, res.Code)
	body := res.Body.String()
	for _, want := range []string{
		"Artist - Album", "manual", `value="42"`, "2.0 kB/s",
		`data-key="rule-` + itoa(audio.ID) + `" open style="--rule: #ff8800"`, "🎵 Audio",
		"~1m left", "done in ~1m", "stalled",
		`data-key="none"`, "No rule",
		`title="/data/torrents/red/audio/Artist - Album"`,
	} {
		require.Contains(t, body, want)
	}
}

func TestTorrentsPageTies(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()
	_, err := h.store.SaveRule(t.Context(), rules.Rule{
		Name: "🎧 Lossless", Color: "#007aff",
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}}},
	})
	require.NoError(t, err)
	require.NoError(t, h.store.Assign(t.Context(), store.Assignment{Hash: "aaa", RuleID: audio.ID}))
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album("aaa", root+"/red/audio")}, nil).Twice()

	require.Contains(t, h.get("/torrents/aaa").Body.String(), "match equally; choose one below")
	body := h.get("/").Body.String()
	require.Contains(t, body, `data-key="ties"`)
	require.Contains(t, body, "⚠ Choose a rule")
	require.Contains(t, body, `<div class="tie">⚠ Matches`)
	require.Contains(t, body, `🎧 Lossless</span>, <span class="badge" style="--rule: #ff8800"><span class="dot"></span>🎵 Audio</span> equally`, "the tie names both rules")
	require.NotContains(t, body, `data-key="rule-`+itoa(audio.ID)+`"`, "a tie is not shown under the rule it was placed by")
}

func TestDashboard(t *testing.T) {
	h := newHarness(t)
	h.audioRule()
	require.NoError(t, h.store.SetTracker(t.Context(), "flacsfor.me", "red"))
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album("aaa", "/dl")}, nil).Twice()

	body := h.get("/").Body.String()
	require.Contains(t, body, `sse-connect="/events"`)
	require.Contains(t, body, `data-section="rules"`)
	require.Contains(t, body, `data-section="trackers"`)
	require.Contains(t, body, `<div id="modal-body"></div>`, "no dialog over the dashboard itself")

	fragment := h.get("/dashboard").Body.String()
	require.Contains(t, fragment, `sse-connect="/events"`)
	require.NotContains(t, fragment, "<html", "swapped into a page that already has a layout")

	for _, u := range []string{"/rules", "/trackers"} {
		res := h.get(u)
		require.Equal(t, http.StatusSeeOther, res.Code, u)
		require.Equal(t, "/", res.Header().Get("Location"), u)
	}
}

// A dialog opened directly loads the dashboard behind it separately, so
// opening one over a loaded dashboard never asks the client for torrents.
func TestDialogPageLoadsDashboardLazily(t *testing.T) {
	h := newHarness(t)
	body := h.get("/rules/new").Body.String()
	require.Contains(t, body, `hx-get="/dashboard" hx-trigger="load"`)
	require.Contains(t, body, `id="rule-form"`)
	require.Contains(t, body, `class="button modal-close" href="/"`)
}

func TestSidePanelsSurviveClientDown(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, context.DeadlineExceeded).Twice()

	body := h.get("/").Body.String()
	require.Contains(t, body, "Cannot reach the torrent client")
	require.Contains(t, body, audio.Name)
	require.Contains(t, h.get("/side").Body.String(), audio.Name)
}

func TestTorrentsPageClientDown(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, context.DeadlineExceeded).Once()

	res := h.get("/")
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), "Cannot reach the torrent client")
}

func TestEvents(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album("aaa", root+"/red/audio")}, nil)
	require.Contains(t, firstEvent(t, h, "/events"), "Artist - Album")
}

// firstEvent returns the first list event the stream at path sends.
func firstEvent(t *testing.T, h *harness, path string) string {
	t.Helper()
	srv := httptest.NewServer(h.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, res.Body.Close()) }()
	require.Equal(t, "text/event-stream", res.Header.Get("Content-Type"))

	sc := bufio.NewScanner(res.Body)
	require.True(t, sc.Scan())
	require.Equal(t, "event: list", sc.Text())
	var data strings.Builder
	for sc.Scan() && sc.Text() != "" {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		require.True(t, ok, "every payload line is a data line: %q", sc.Text())
		data.WriteString(line)
	}
	return data.String()
}

// ruleValues is the editor form for a rule matching any of extension mkv or
// mp4, in a single top-level group "top".
func ruleValues(name string, extra url.Values) url.Values {
	f := url.Values{
		"name":         {name},
		"color":        {"#123456"},
		"path":         {"{root}/{tracker}/video"},
		"group":        {"top"},
		"group_parent": {""},
		"group_mode":   {"any"},
		"cond_group":   {"top", "top"},
		"field":        {"extension", "extension"},
		"op":           {"is", "is"},
		"value":        {"mkv", " mp4 "},
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func TestCreateRule(t *testing.T) {
	h := newHarness(t)
	h.audioRule()

	res := h.post("/rules/new", ruleValues("🎬 Video", nil))
	require.Equal(t, http.StatusSeeOther, res.Code)
	require.Equal(t, "/", res.Header().Get("Location"))

	rs := h.rules()
	require.Len(t, rs, 2)
	got := rs[0]
	got.ID = 0
	require.Equal(t, rules.Rule{
		Name: "🎬 Video", Color: "#123456", Path: "{root}/{tracker}/video",
		Match: rules.Group{Mode: rules.ModeAny, Conditions: []rules.Condition{
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mkv"},
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mp4"},
		}},
	}, got)
}

func TestCreateRuleInvalid(t *testing.T) {
	h := newHarness(t)
	h.audioRule()

	tests := []struct {
		name string
		form url.Values
		want string
	}{
		{name: "comma", form: ruleValues("a,b", nil), want: "name cannot contain commas"},
		{name: "duplicate", form: ruleValues("🎵 Audio", nil), want: "another rule already has that name"},
		{name: "empty value", form: ruleValues("x", url.Values{"value": {"mkv", ""}}), want: "condition 2: value is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := h.post("/rules/new", tt.form)
			require.Equal(t, http.StatusUnprocessableEntity, res.Code)
			require.Contains(t, res.Body.String(), tt.want)
			require.Len(t, h.rules(), 1)
		})
	}
}

func TestEditRule(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()

	res := h.get("/rules/" + itoa(audio.ID))
	require.Equal(t, http.StatusOK, res.Code)
	body := res.Body.String()
	require.Contains(t, body, `value="🎵 Audio"`)
	require.Contains(t, body, `popovertarget="emoji-popover"`)
	require.Contains(t, body, `<emoji-picker data-source="/static/emoji/data.json" data-emoji-for="rule-name">`)

	res = h.post("/rules/"+itoa(audio.ID), ruleValues("🎵 Music", nil))
	require.Equal(t, http.StatusSeeOther, res.Code)
	rs := h.rules()
	require.Len(t, rs, 1)
	require.Equal(t, "🎵 Music", rs[0].Name)

	require.Equal(t, http.StatusNotFound, h.get("/rules/999").Code)
	require.Equal(t, http.StatusNotFound, h.post("/rules/999", ruleValues("x", nil)).Code)
}

func TestParseGroups(t *testing.T) {
	cond := func(field rules.Field, value string) rules.Condition {
		return rules.Condition{Field: field, Op: rules.OpIs, Value: value}
	}
	// Rows arrive in DOM order, so a nested group's rows can come before
	// later rows of its parent.
	f := url.Values{
		"group":        {"top", "g2", "orphan", "loop", "top"},
		"group_parent": {"", "top", "missing", "loop", "g2"},
		"group_mode":   {"all", "any", "any", "any", "none"},
		"cond_group":   {"top", "g2", "g2", "orphan", "loop", "top"},
		"field":        {"tracker", "extension", "extension", "name", "name", "name"},
		"op":           {"is", "is", "is", "is", "is", "is"},
		"value":        {"torrentleech.org", "mkv", " mp4 ", "lost", "lost", "sample"},
	}
	require.Equal(t, rules.Group{
		Mode:       rules.ModeAll,
		Conditions: []rules.Condition{cond(rules.FieldTracker, "torrentleech.org"), cond(rules.FieldName, "sample")},
		Groups: []rules.Group{{
			Mode:       rules.ModeAny,
			Conditions: []rules.Condition{cond(rules.FieldExtension, "mkv"), cond(rules.FieldExtension, "mp4")},
		}},
	}, parseGroups(f), "orphans, self-parented groups and duplicate IDs are dropped")

	require.Equal(t, rules.Group{Mode: rules.ModeAll}, parseGroups(url.Values{}), "no groups is an empty rule")
}

func TestNestedRuleEditor(t *testing.T) {
	h := newHarness(t)
	form := ruleValues("🎬 Video", url.Values{
		"group":        {"top", "inner"},
		"group_parent": {"", "top"},
		"group_mode":   {"all", "any"},
		"cond_group":   {"top", "inner", "inner"},
		"field":        {"tracker", "extension", "extension"},
		"op":           {"is", "is", "is"},
		"value":        {"torrentleech.org", "mkv", "mp4"},
	})
	require.Equal(t, http.StatusSeeOther, h.post("/rules/new", form).Code)
	video := h.rules()[0]
	require.Equal(t, "tracker is torrentleech.org and (extension is mkv or extension is mp4)", video.Match.String())

	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
	require.Contains(t, h.get("/side").Body.String(), "<code>tracker is torrentleech.org and (extension is mkv or extension is mp4)</code>")
	body := h.get("/rules/" + itoa(video.ID)).Body.String()
	require.Contains(t, body, `<fieldset class="cond-group nested">`)
	require.Contains(t, body, `<option value="none" >none</option>`)
	require.Equal(t, 3, strings.Count(body, `name="cond_group"`))

	res := h.post("/rules/new", ruleValues("Empty", url.Values{
		"group": {"top", "inner"}, "group_parent": {"", "top"}, "group_mode": {"all", "any"},
	}))
	require.Equal(t, http.StatusUnprocessableEntity, res.Code)
	require.Contains(t, res.Body.String(), "nested groups need at least one condition")
}

func TestAddGroupAndCondition(t *testing.T) {
	h := newHarness(t)
	body := h.get("/rules/group?parent=abc").Body.String()
	require.Contains(t, body, `name="group_parent" value="abc"`)
	require.Contains(t, body, `<option value="any" selected>any</option>`)
	id := body[strings.Index(body, `name="group" value="`)+len(`name="group" value="`):]
	id = id[:strings.Index(id, `"`)]
	require.Contains(t, body, `name="cond_group" value="`+id+`"`, "the new group comes with one condition of its own")

	require.Contains(t, h.get("/rules/condition?group=abc").Body.String(), `name="cond_group" value="abc"`)
}

func TestDuplicateAndDeleteRule(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()
	require.Equal(t, http.StatusSeeOther, h.post("/rules/new", ruleValues("🎬 Video", nil)).Code)
	video := h.rules()[0]

	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
	require.Contains(t, h.get("/side").Body.String(), `href="/rules/new?from=`+itoa(audio.ID)+`" aria-label="Duplicate 🎵 Audio">Duplicate</a>`)
	require.Contains(t, h.get("/rules/"+itoa(audio.ID)).Body.String(), `href="/rules/new?from=`+itoa(audio.ID)+`"`)
	body := h.get("/rules/new?from=" + itoa(audio.ID)).Body.String()
	require.Contains(t, body, `value="🎵 Audio copy"`)
	require.Contains(t, body, `action="/rules/new"`, "the copy saves as a new rule")
	require.Contains(t, body, `value="flac"`)
	require.Equal(t, http.StatusNotFound, h.get("/rules/new?from=999").Code)

	require.Equal(t, http.StatusSeeOther, h.post("/rules/"+itoa(video.ID)+"/delete", nil).Code)
	require.Equal(t, []int64{audio.ID}, ids(h.rules()))
}

func movie(hash, name, dir string) torrent.Status {
	return torrent.Status{
		Torrent: torrent.Torrent{
			Hash: hash, Name: name,
			Trackers: []string{"https://flacsfor.me/x/announce"},
			Files:    []torrent.File{{Path: name + "/movie.mkv", Size: 100}},
		},
		Dir: dir,
	}
}

// library sets up an album placed by the audio rule, a movie recorded in
// place and a movie the user pinned by hand.
func (h *harness) library() (rules.Rule, []torrent.Status) {
	h.t.Helper()
	audio := h.audioRule()
	placed := album("flac", root+"/red/audio")
	placed.Group = audio.Name
	statuses := []torrent.Status{placed, movie("mkv", "Movie", "/dl"), movie("pinned", "Pinned", "/mine")}
	for _, a := range []store.Assignment{
		{Hash: "flac", RuleID: audio.ID},
		{Hash: "mkv"},
		{Hash: "pinned", Manual: true},
	} {
		require.NoError(h.t, h.store.Assign(h.t.Context(), a))
	}
	return audio, statuses
}

// card is the summary of a preview card titled title holding n torrents.
func card(title string, n int) string {
	return `<span class="layer-title">` + title + `</span><span class="count">` + strconv.Itoa(n) + `</span>`
}

func TestPreviewNewRule(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	body := h.post("/rules/preview", ruleValues("🎬 Video", url.Values{"value": {"mkv", "flac"}})).Body.String()
	require.Contains(t, body, card("Matched", 1))
	require.Contains(t, body, `<a class="name" href="/torrents/mkv"`)
	require.Contains(t, body, `⚠ Tied with other rules</span><span class="count">1</span>`, "audio and video both match the album by one condition")
	require.Contains(t, body, `<a class="name" href="/torrents/flac"`)
	require.Contains(t, body, "1 more match, but a more specific rule or a manual pin takes it.", "the pinned movie")
	require.Contains(t, body, card("Move on save", 1), "the pinned movie is listed but not moved")
	require.Contains(t, body, `name="move" value="mkv" form="rule-form" checked`)
	require.NotContains(t, body, `name="move" value="flac"`, "a tie never moves")
	require.Contains(t, body, "<code>/dl</code> → <code>/data/torrents/red/video</code>")
	require.Contains(t, body, "group: none → 🎬 Video")
	require.Contains(t, body, "Pinned</span> <span class=\"tag\">manual, not moved</span>")
}

func TestPreviewFolds(t *testing.T) {
	h := newHarness(t)
	var statuses []torrent.Status
	for i := range 7 {
		hash := "m" + strconv.Itoa(i)
		statuses = append(statuses, movie(hash, "Movie "+strconv.Itoa(i), "/dl"))
		require.NoError(t, h.store.Assign(t.Context(), store.Assignment{Hash: hash}))
	}
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	body := h.post("/rules/preview", ruleValues("🎬 Video", nil)).Body.String()
	require.Contains(t, body, card("Matched", 7))
	require.Equal(t, 2, strings.Count(body, `<details class="more"><summary>2 more</summary>`), "both cards fold after 5")
	require.Equal(t, 7, strings.Count(body, `name="move"`), "folded rows keep their checkboxes")
	head, _, _ := strings.Cut(body, `<details class="more">`)
	require.Contains(t, head, "Movie 4")
	require.NotContains(t, head, "Movie 5")
}

func TestPreviewPinnedMatch(t *testing.T) {
	h := newHarness(t)
	audio, statuses := h.library()
	require.NoError(t, h.store.Assign(t.Context(), store.Assignment{Hash: "mkv", RuleID: audio.ID, Manual: true}))
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	form := ruleValues(audio.Name, url.Values{
		"id": {itoa(audio.ID)}, "group_mode": {"all"}, "path": {"{root}/{tracker}/audio"},
		"cond_group": {"top"}, "field": {"extension"}, "op": {"is"}, "value": {"flac"},
	})
	body := h.post("/rules/preview", form).Body.String()
	require.Contains(t, body, card("Matched", 2))
	require.Contains(t, body, `<a class="name" href="/torrents/flac"`)
	require.Contains(t, body, `<a class="name" href="/torrents/mkv"`)
	require.Contains(t, body, `<span class="tag">manual</span>`)
	require.NotContains(t, body, "Move on save", "a pinned torrent is never moved")
}

func TestPreviewEditedRule(t *testing.T) {
	h := newHarness(t)
	audio, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	form := ruleValues(audio.Name, url.Values{
		"id": {itoa(audio.ID)}, "group_mode": {"all"}, "path": {"{root}/{tracker}/music"},
		"cond_group": {"top"}, "field": {"extension"}, "op": {"is"}, "value": {"flac"},
	})
	body := h.post("/rules/preview", form).Body.String()
	require.Contains(t, body, "<code>/data/torrents/red/audio</code> → <code>/data/torrents/red/music</code>")
	require.NotContains(t, body, "Movie")
}

func TestPreviewNoChanges(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	body := h.post("/rules/preview", ruleValues("x", url.Values{"field": nil})).Body.String()
	require.Contains(t, body, "only assigned by hand")
	require.Contains(t, body, card("Matches", 0))
	require.Contains(t, body, "No torrents match.")
	require.NotContains(t, body, "Move on save")
}

func TestSaveRuleMovesSelected(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	extra := movie("skip", "Skipped", "/dl")
	statuses = append(statuses, extra)
	require.NoError(t, h.store.Assign(t.Context(), store.Assignment{Hash: "skip"}))

	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Twice()
	h.client.EXPECT().Move(mock.Anything, "mkv", root+"/red/video").Return(nil).Once()
	h.client.EXPECT().SetGroup(mock.Anything, "mkv", "🎬 Video").Return(nil).Once()

	res := h.post("/rules/new", ruleValues("🎬 Video", url.Values{"move": {"mkv", "pinned", "flac"}}))
	require.Equal(t, http.StatusSeeOther, res.Code)

	video := h.rules()[0]
	got, err := h.store.Assignments(t.Context())
	require.NoError(t, err)
	require.Equal(t, store.Assignment{Hash: "mkv", RuleID: video.ID}, got["mkv"])
	require.Equal(t, store.Assignment{Hash: "pinned", Manual: true}, got["pinned"], "manual torrents are never moved")
	require.Equal(t, store.Assignment{Hash: "skip"}, got["skip"], "unchecked torrents stay")
}

func TestSaveRuleMoveFails(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Twice()
	h.client.EXPECT().Move(mock.Anything, "mkv", root+"/red/video").Return(context.DeadlineExceeded).Once()

	res := h.post("/rules/new", ruleValues("🎬 Video", url.Values{"move": {"mkv"}}))
	require.Equal(t, http.StatusBadGateway, res.Code)
	require.Contains(t, res.Body.String(), "Some torrents could not be moved")
	require.Len(t, h.rules(), 2)
}

func TestTorrentPage(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Times(2)

	body := h.get("/torrents/flac").Body.String()
	require.Contains(t, body, "Artist - Album")
	require.Contains(t, body, `<option value="auto" selected>`)
	require.Contains(t, body, `<button class="primary" disabled>Save</button>`, "already where the rules want it")

	body = h.post("/torrents/flac/choose", url.Values{"rule": {"none"}}).Body.String()
	require.Contains(t, body, `<option value="none" selected>`)
	require.Contains(t, body, "<code>/data/torrents/red</code>")
	require.Contains(t, body, `<button class="primary" >Save</button>`)
}

func TestTorrentPageSave(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Twice()
	h.client.EXPECT().Move(mock.Anything, "flac", root+"/red").Return(nil).Once()
	h.client.EXPECT().SetGroup(mock.Anything, "flac", "").Return(nil).Once()

	res := h.post("/torrents/flac", url.Values{"rule": {"none"}})
	require.Equal(t, http.StatusSeeOther, res.Code)
	got, err := h.store.Assignments(t.Context())
	require.NoError(t, err)
	require.Equal(t, store.Assignment{Hash: "flac", Manual: true}, got["flac"])
}

// roms is a torrent with three files, the first downloaded, the second
// half done and the third not selected.
func roms() torrent.Status {
	return torrent.Status{
		Torrent: torrent.Torrent{Hash: "psx", Name: "PlayStation", Files: []torrent.File{
			{Path: "PlayStation/crash.chd", Size: 100, Index: 0, Wanted: true, Done: 100},
			{Path: "PlayStation/spyro.chd", Size: 100, Index: 1, Wanted: true, Done: 50},
			{Path: "PlayStation/tekken.chd", Size: 200, Index: 2},
		}},
		Dir: root, State: torrent.StateDownloading,
	}
}

func TestTorrentPageFiles(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{roms()}, nil).Once()

	body := h.get("/torrents/psx").Body.String()
	require.Contains(t, body, `action="/torrents/psx/files"`)
	require.Contains(t, body, `<span class="count" data-summary>2 of 3 · 200 B</span>`)
	require.Contains(t, body, `data-filter-files>`)
	require.Contains(t, body, `<li data-path="PlayStation/crash.chd" data-size="100">`)
	require.Contains(t, body, `<input type="checkbox" data-toggle-files >`, "not every file is selected")
	require.Contains(t, body, `<input type="checkbox" name="file" value="1" checked> <span>PlayStation/spyro.chd</span></label>
      <span class="count">100 B · 50%</span>`)
	require.Contains(t, body, `<input type="checkbox" name="file" value="2" > <span>PlayStation/tekken.chd</span></label>
      <span class="count">200 B</span>`)
}

func TestChooseFiles(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		wanted   []int
		unwanted []int
		code     int
		body     string
	}{
		{name: "add and drop", files: []string{"0", "2"}, wanted: []int{2}, unwanted: []int{1}, code: http.StatusSeeOther},
		{name: "unchanged", files: []string{"0", "1"}, code: http.StatusSeeOther},
		{name: "none", code: http.StatusUnprocessableEntity, body: "Select at least one file to download."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			calls := 1
			if tt.code != http.StatusSeeOther {
				calls = 2
			}
			h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{roms()}, nil).Times(calls)
			if tt.wanted != nil || tt.unwanted != nil {
				h.client.EXPECT().SetWanted(mock.Anything, "psx", tt.wanted, tt.unwanted).Return(nil).Once()
			}

			res := h.post("/torrents/psx/files", url.Values{"file": tt.files})
			require.Equal(t, tt.code, res.Code)
			require.Contains(t, res.Body.String(), tt.body)
			if tt.code == http.StatusSeeOther {
				require.Equal(t, "/torrents/psx", res.Header().Get("Location"))
			}
		})
	}
}

func TestTorrentPageUnknown(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
	require.Equal(t, http.StatusNotFound, h.get("/torrents/nope").Code)
}

func TestTrackers(t *testing.T) {
	h := newHarness(t)
	other := album("aaa", "/dl")
	other.Trackers = []string{"udp://new.example:80", "https://flacsfor.me/x"}
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{other}, nil).Once()

	body := h.get("/side").Body.String()
	require.Contains(t, body, `value="red"`)
	require.Contains(t, body, `<code title="new.example">new.example</code> <span class="tag">seen</span>`)

	require.Equal(t, http.StatusSeeOther, h.post("/trackers", url.Values{"host": {" New.Example "}, "name": {"new"}}).Code)
	require.Equal(t, http.StatusSeeOther, h.post("/trackers/delete", url.Values{"host": {"flacsfor.me"}}).Code)
	trackers, err := h.store.Trackers(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"new.example": "new"}, trackers)
}

func TestTrackersInvalid(t *testing.T) {
	h := newHarness(t)

	res := h.post("/trackers", url.Values{"host": {"x.example"}, "name": {"a/b"}})
	require.Equal(t, http.StatusUnprocessableEntity, res.Code)
	require.Contains(t, res.Body.String(), "cannot contain /")
}

func albumTorrent(t *testing.T) []byte {
	t.Helper()
	return torrenttest.Encode(t, metainfo.Info{Name: "Artist - Album", Files: []metainfo.FileInfo{
		{Path: []string{"01.flac"}, Length: 300},
		{Path: []string{"cover.jpg"}, Length: 2},
	}}, metainfo.AnnounceList{{"https://flacsfor.me/x/announce"}})
}

func multipartUpload(t *testing.T, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("torrent", "album.torrent")
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/add/preview", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// preview follows a hand-off redirect to the preview page.
func (h *harness) preview(res *httptest.ResponseRecorder) (string, string) {
	h.t.Helper()
	require.Equal(h.t, http.StatusSeeOther, res.Code)
	loc := res.Header().Get("Location")
	require.True(h.t, strings.HasPrefix(loc, "/add/"), loc)
	page := h.get(loc)
	require.Equal(h.t, http.StatusOK, page.Code)
	return loc, page.Body.String()
}

func TestAddPreview(t *testing.T) {
	h := newHarness(t)
	h.audioRule()
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	loc, body := h.preview(h.do(multipartUpload(t, albumTorrent(t))))
	for _, want := range []string{
		"Artist - Album", "302 B in 2 files", "/data/torrents/red/audio", "🎵 Audio", "Add to client",
		`name="file" value="0" checked`, `name="file" value="1" checked`,
		`href="/rules/new?return=` + loc + `"`,
		`name="upload" value="` + strings.TrimPrefix(loc, "/add/") + `"`,
	} {
		require.Contains(t, body, want)
	}
}

func TestAddPreviewInvalid(t *testing.T) {
	h := newHarness(t)
	res := h.do(multipartUpload(t, []byte("not a torrent")))
	require.Equal(t, http.StatusUnprocessableEntity, res.Code)
	require.Contains(t, res.Body.String(), "not a valid .torrent file")
}

func TestAddPreviewDuplicate(t *testing.T) {
	h := newHarness(t)
	data := albumTorrent(t)
	parsed, err := torrent.Parse(bytes.NewReader(data))
	require.NoError(t, err)
	h.client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album(parsed.Hash, "/dl")}, nil).Once()

	_, body := h.preview(h.do(multipartUpload(t, data)))
	require.Contains(t, body, "already in the client")
	require.NotContains(t, body, "Add to client")
}

func TestUploadsAPI(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	req := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader(albumTorrent(t)))
	req.Header.Set("Content-Type", "application/x-bittorrent")
	res := h.do(req)
	require.Equal(t, http.StatusCreated, res.Code)
	var created struct{ URL string }
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &created))
	require.True(t, strings.HasPrefix(created.URL, "/add/"), created.URL)
	require.Contains(t, h.get(created.URL).Body.String(), "Artist - Album")

	bad := h.do(httptest.NewRequest(http.MethodPost, "/api/uploads", strings.NewReader("junk")))
	require.Equal(t, http.StatusBadRequest, bad.Code)

	gone := h.get("/add/nope")
	require.Equal(t, http.StatusNotFound, gone.Code)
	require.Contains(t, gone.Body.String(), "That upload has expired")
}

func addForm(data []byte, rule string, files ...string) url.Values {
	return url.Values{"metainfo": {base64.StdEncoding.EncodeToString(data)}, "rule": {rule}, "file": files}
}

func TestChooseKeepsSelection(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	body := h.post("/add/choose", addForm(albumTorrent(t), "none", "1")).Body.String()
	require.Contains(t, body, `name="file" value="0" >`)
	require.Contains(t, body, `name="file" value="1" checked`)
	require.Contains(t, body, `<option value="none" selected>`)
	require.Contains(t, body, "<code>/data/torrents/red</code>")
}

func TestAdd(t *testing.T) {
	data := albumTorrent(t)
	parsed, err := torrent.Parse(bytes.NewReader(data))
	require.NoError(t, err)

	tests := []struct {
		name       string
		choice     func(audio rules.Rule) string
		files      []string
		wantDir    string
		wantGroup  string
		wantSkip   []int
		wantRule   bool
		wantManual bool
	}{
		{name: "automatic", choice: func(rules.Rule) string { return "auto" }, files: []string{"0", "1"}, wantDir: root + "/red/audio", wantGroup: "🎵 Audio", wantRule: true},
		{name: "chosen rule", choice: func(a rules.Rule) string { return itoa(a.ID) }, files: []string{"0", "1"}, wantDir: root + "/red/audio", wantGroup: "🎵 Audio", wantRule: true, wantManual: true},
		{name: "no rule", choice: func(rules.Rule) string { return "none" }, files: []string{"0", "1"}, wantDir: root + "/red", wantManual: true},
		{name: "skipped file", choice: func(rules.Rule) string { return "auto" }, files: []string{"0"}, wantDir: root + "/red/audio", wantGroup: "🎵 Audio", wantSkip: []int{1}, wantRule: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			audio := h.audioRule()
			h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
			h.client.EXPECT().Add(mock.Anything, torrent.Addition{
				Metainfo: data, Dir: tt.wantDir, Group: tt.wantGroup, Skip: tt.wantSkip,
			}).Return(parsed.Hash, nil).Once()

			res := h.post("/add", addForm(data, tt.choice(audio), tt.files...))
			require.Equal(t, http.StatusSeeOther, res.Code)

			want := store.Assignment{Hash: parsed.Hash, Manual: tt.wantManual}
			if tt.wantRule {
				want.RuleID = audio.ID
			}
			got, err := h.store.Assignments(t.Context())
			require.NoError(t, err)
			require.Equal(t, map[string]store.Assignment{parsed.Hash: want}, got)
		})
	}
}

func TestAddNoFiles(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

	res := h.post("/add", addForm(albumTorrent(t), "auto"))
	require.Equal(t, http.StatusUnprocessableEntity, res.Code)
	require.Contains(t, res.Body.String(), "Select at least one file")
}

func TestRuleEditorReturn(t *testing.T) {
	h := newHarness(t)
	body := h.get("/rules/new?return=/add/abc").Body.String()
	require.Contains(t, body, `name="return" value="/add/abc"`)
	require.Contains(t, body, `href="/add/abc" class="button">Cancel`)

	res := h.post("/rules/new", ruleValues("🎬 Video", url.Values{"return": {"/add/abc"}}))
	require.Equal(t, "/add/abc", res.Header().Get("Location"))

	for _, bad := range []string{"https://evil.example", "//evil.example", "/add/abc/../../x", "/rules"} {
		res := h.post("/rules/new", ruleValues("x"+bad, url.Values{"return": {bad}, "name": {"r" + itoa(int64(len(bad)))}}))
		require.Equal(t, "/", res.Header().Get("Location"), bad)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/rules/new", strings.NewReader(ruleValues("x", nil).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	require.Equal(t, http.StatusForbidden, h.do(req).Code)
	require.Empty(t, h.rules())
}

func TestStaticAssets(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{
		"/static/htmx.min.js", "/static/sse.js", "/static/app.css", "/sw.js", "/manifest.webmanifest",
		"/static/emoji/picker.js", "/static/emoji/database.js", "/static/emoji/data.json",
	} {
		require.Equal(t, http.StatusOK, h.get(p).Code, p)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func ids(rs []rules.Rule) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
