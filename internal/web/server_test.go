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
	srv, err := New(client, st, intake.NewReconciler(client, st, root), root, 10*time.Millisecond, reveal)
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
		Name: "🎵 Audio", Color: "#ff8800", Priority: 1, Mode: rules.ModeAll,
		Path:       "{root}/{tracker}/audio",
		Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}},
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
	}, groups(down), "rules in priority order, paused torrents stay, no rule last")

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

	srv := httptest.NewServer(h.h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
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
	require.Contains(t, data.String(), "Artist - Album")
}

func ruleValues(name string, extra url.Values) url.Values {
	f := url.Values{
		"name":  {name},
		"color": {"#123456"},
		"mode":  {"any"},
		"path":  {"{root}/{tracker}/video"},
		"field": {"extension", "extension"},
		"op":    {"is", "is"},
		"value": {"mkv", " mp4 "},
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
	require.Equal(t, "/rules", res.Header().Get("Location"))

	rs := h.rules()
	require.Len(t, rs, 2)
	got := rs[1]
	got.ID = 0
	require.Equal(t, rules.Rule{
		Name: "🎬 Video", Color: "#123456", Priority: 2, Mode: rules.ModeAny, Path: "{root}/{tracker}/video",
		Conditions: []rules.Condition{
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mkv"},
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mp4"},
		},
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
	require.Equal(t, audio.Priority, rs[0].Priority)

	require.Equal(t, http.StatusNotFound, h.get("/rules/999").Code)
	require.Equal(t, http.StatusNotFound, h.post("/rules/999", ruleValues("x", nil)).Code)
}

func TestMoveAndDeleteRule(t *testing.T) {
	h := newHarness(t)
	audio := h.audioRule()
	require.Equal(t, http.StatusSeeOther, h.post("/rules/new", ruleValues("🎬 Video", nil)).Code)
	video := h.rules()[1]

	require.Equal(t, http.StatusSeeOther, h.post("/rules/"+itoa(video.ID)+"/move", url.Values{"dir": {"up"}}).Code)
	require.Equal(t, []int64{video.ID, audio.ID}, ids(h.rules()))
	require.Equal(t, http.StatusSeeOther, h.post("/rules/"+itoa(video.ID)+"/move", url.Values{"dir": {"up"}}).Code)
	require.Equal(t, []int64{video.ID, audio.ID}, ids(h.rules()), "moving the first rule up is a no-op")

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

func TestPreviewNewRule(t *testing.T) {
	h := newHarness(t)
	_, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	body := h.post("/rules/preview", ruleValues("🎬 Video", url.Values{"value": {"mkv", "flac"}})).Body.String()
	require.Contains(t, body, `name="move" value="mkv" form="rule-form" checked`)
	require.Contains(t, body, "<code>/dl</code> → <code>/data/torrents/red/video</code>")
	require.Contains(t, body, "group: none → 🎬 Video")
	require.Contains(t, body, "Pinned</span> <span class=\"tag\">manual, not moved</span>")
	require.NotContains(t, body, "Artist - Album", "the higher-priority audio rule keeps the album")
}

func TestPreviewEditedRule(t *testing.T) {
	h := newHarness(t)
	audio, statuses := h.library()
	h.client.EXPECT().Torrents(mock.Anything).Return(statuses, nil).Once()

	form := ruleValues(audio.Name, url.Values{
		"id": {itoa(audio.ID)}, "mode": {"all"}, "path": {"{root}/{tracker}/music"},
		"field": {"extension"}, "op": {"is"}, "value": {"flac"},
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
	require.Contains(t, body, "Saving won't move any torrents")
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

	video := h.rules()[1]
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
	require.Contains(t, res.Body.String(), "Rule saved, but some torrents could not be moved")
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

	body := h.get("/trackers").Body.String()
	require.Contains(t, body, `value="red"`)
	require.Contains(t, body, "<code>new.example</code> <span class=\"tag\">seen</span>")

	require.Equal(t, http.StatusSeeOther, h.post("/trackers", url.Values{"host": {" New.Example "}, "name": {"new"}}).Code)
	require.Equal(t, http.StatusSeeOther, h.post("/trackers/delete", url.Values{"host": {"flacsfor.me"}}).Code)
	trackers, err := h.store.Trackers(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"new.example": "new"}, trackers)
}

func TestTrackersInvalid(t *testing.T) {
	h := newHarness(t)
	h.client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()

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
		require.Equal(t, "/rules", res.Header().Get("Location"), bad)
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
