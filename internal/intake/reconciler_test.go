package intake

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
	"github.com/cwood/torrent-org/internal/torrent/torrentmock"
)

const root = "/data/torrents"

func status(hash, name, dir, group string, files ...string) torrent.Status {
	s := torrent.Status{
		Torrent: torrent.Torrent{Hash: hash, Name: name, Trackers: []string{"https://flacsfor.me/x/announce"}},
		Dir:     dir,
		Group:   group,
	}
	for _, f := range files {
		s.Files = append(s.Files, torrent.File{Path: name + "/" + f, Size: 100})
	}
	return s
}

// setup returns a store with a baseline taken (unless fresh), a tracker alias
// and a single "🎵 Audio" rule matching flac.
func setup(t *testing.T, fresh bool) (*store.Store, int64) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	require.NoError(t, st.SetTracker(ctx, "flacsfor.me", "red"))
	id, err := st.SaveRule(ctx, rules.Rule{
		Name: "🎵 Audio", Color: "#ff8800", Path: "{root}/{tracker}/audio",
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}}},
	})
	require.NoError(t, err)
	if !fresh {
		require.NoError(t, st.RecordBaseline(ctx, nil))
	}
	return st, id
}

func assignments(t *testing.T, st *store.Store) map[string]store.Assignment {
	t.Helper()
	got, err := st.Assignments(t.Context())
	require.NoError(t, err)
	return got
}

func TestFirstPassRecordsInPlace(t *testing.T) {
	st, _ := setup(t, true)
	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{
		status("aaa", "Album", "/home/me/Downloads", "", "01.flac"),
		status("bbb", "Movie", "/home/me/Downloads", "", "movie.mkv"),
	}, nil).Once()

	require.NoError(t, NewReconciler(client, st, root).Reconcile(t.Context()))

	require.Equal(t, map[string]store.Assignment{"aaa": {Hash: "aaa"}, "bbb": {Hash: "bbb"}}, assignments(t, st))
	ok, err := st.HasBaseline(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestPlacesNewTorrents(t *testing.T) {
	st, audioID := setup(t, false)
	require.NoError(t, st.Assign(t.Context(), store.Assignment{Hash: "old", Manual: true}))

	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{
		status("old", "Old", "/anywhere", "whatever", "01.flac"),
		status("new", "Album", "/home/me/Downloads", "", "01.flac", "cover.jpg"),
		status("placed", "Done", root+"/red/audio", "🎵 Audio", "01.flac"),
		status("other", "Movie", "/home/me/Downloads", "stale", "movie.mkv"),
		status("magnet", "Fetching", "/home/me/Downloads", ""),
	}, nil).Once()
	client.EXPECT().Move(mock.Anything, "new", root+"/red/audio").Return(nil).Once()
	client.EXPECT().SetGroup(mock.Anything, "new", "🎵 Audio").Return(nil).Once()
	client.EXPECT().Move(mock.Anything, "other", root+"/red").Return(nil).Once()
	client.EXPECT().SetGroup(mock.Anything, "other", "").Return(nil).Once()

	require.NoError(t, NewReconciler(client, st, root).Reconcile(t.Context()))

	require.Equal(t, map[string]store.Assignment{
		"old":    {Hash: "old", Manual: true},
		"new":    {Hash: "new", RuleID: audioID},
		"placed": {Hash: "placed", RuleID: audioID},
		"other":  {Hash: "other"},
	}, assignments(t, st))
}

func TestForgetsRemovedTorrents(t *testing.T) {
	st, _ := setup(t, false)
	require.NoError(t, st.Assign(t.Context(), store.Assignment{Hash: "gone"}))
	require.NoError(t, st.Assign(t.Context(), store.Assignment{Hash: "kept"}))

	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{
		status("kept", "Kept", "/x", "", "a.flac"),
	}, nil).Once()

	require.NoError(t, NewReconciler(client, st, root).Reconcile(t.Context()))
	require.Equal(t, map[string]store.Assignment{"kept": {Hash: "kept"}}, assignments(t, st))
}

func TestFailedPlacementRetriedNextPass(t *testing.T) {
	st, audioID := setup(t, false)
	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{
		status("bad", "Bad", "/dl", "", "01.flac"),
		status("good", "Good", "/dl", "", "01.flac"),
	}, nil).Once()
	client.EXPECT().Move(mock.Anything, "bad", root+"/red/audio").Return(errors.New("disk full")).Once()
	client.EXPECT().Move(mock.Anything, "good", root+"/red/audio").Return(nil).Once()
	client.EXPECT().SetGroup(mock.Anything, "good", "🎵 Audio").Return(nil).Once()

	err := NewReconciler(client, st, root).Reconcile(t.Context())
	require.ErrorContains(t, err, "place Bad (bad): move to /data/torrents/red/audio: disk full")
	require.Equal(t, map[string]store.Assignment{"good": {Hash: "good", RuleID: audioID}}, assignments(t, st))
}

func TestListFailure(t *testing.T) {
	st, _ := setup(t, false)
	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return(nil, errors.New("connection refused")).Once()

	require.ErrorContains(t, NewReconciler(client, st, root).Reconcile(t.Context()), "connection refused")
}

func TestAdd(t *testing.T) {
	st, audioID := setup(t, false)
	rs, err := st.Rules(t.Context())
	require.NoError(t, err)
	p := rules.PlaceIn(torrent.Torrent{Trackers: []string{"https://flacsfor.me/x"}}, &rs[0], map[string]string{"flacsfor.me": "red"}, root)

	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return(nil, nil).Once()
	client.EXPECT().Add(mock.Anything, torrent.Addition{
		Metainfo: []byte("meta"), Dir: root + "/red/audio", Group: "🎵 Audio", Skip: []int{2},
	}).Return("new", nil).Once()

	err = NewReconciler(client, st, root).Add(t.Context(), Addition{
		Metainfo: []byte("meta"), Hash: "new", Placement: p, Skip: []int{2}, Manual: true,
	})
	require.NoError(t, err)
	require.Equal(t, map[string]store.Assignment{"new": {Hash: "new", RuleID: audioID, Manual: true}}, assignments(t, st))
}

func TestAddDuplicate(t *testing.T) {
	st, _ := setup(t, false)
	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{status("dup", "Dup", "/x", "")}, nil).Once()

	err := NewReconciler(client, st, root).Add(t.Context(), Addition{Metainfo: []byte("meta"), Hash: "dup", Placement: rules.Placement{Dir: root}})
	require.ErrorIs(t, err, ErrDuplicate)
	require.Empty(t, assignments(t, st))
}

func TestRelocate(t *testing.T) {
	st, audioID := setup(t, false)
	rs, err := st.Rules(t.Context())
	require.NoError(t, err)
	audio := rules.Placement{Rule: &rs[0], Dir: root + "/red/audio", Group: "🎵 Audio"}

	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{
		status("move", "Move", "/dl", "", "01.flac"),
		status("regroup", "Regroup", root+"/red/audio", "", "01.flac"),
		status("fails", "Fails", "/dl", "", "01.flac"),
	}, nil).Once()
	client.EXPECT().Move(mock.Anything, "move", root+"/red/audio").Return(nil).Once()
	client.EXPECT().SetGroup(mock.Anything, "move", "🎵 Audio").Return(nil).Once()
	client.EXPECT().SetGroup(mock.Anything, "regroup", "🎵 Audio").Return(nil).Once()
	client.EXPECT().Move(mock.Anything, "fails", root+"/red/audio").Return(errors.New("read-only")).Once()

	err = NewReconciler(client, st, root).Relocate(t.Context(), []Move{
		{Hash: "move", Placement: audio, Manual: true},
		{Hash: "regroup", Placement: audio},
		{Hash: "fails", Placement: audio},
		{Hash: "gone", Placement: audio},
	})
	require.ErrorContains(t, err, "relocate Fails: move to /data/torrents/red/audio: read-only")
	require.ErrorIs(t, err, torrent.ErrNotFound)
	require.Equal(t, map[string]store.Assignment{
		"move":    {Hash: "move", RuleID: audioID, Manual: true},
		"regroup": {Hash: "regroup", RuleID: audioID},
	}, assignments(t, st))
}

func TestPlan(t *testing.T) {
	st, audioID := setup(t, false)
	rs, err := st.Rules(t.Context())
	require.NoError(t, err)
	trackers := map[string]string{"flacsfor.me": "red"}

	statuses := []torrent.Status{
		status("in-place", "Recorded", "/dl", "", "01.flac"),
		status("placed", "Placed", root+"/red/audio", "🎵 Audio", "01.flac"),
		status("manual", "Manual", "/elsewhere", "Mine", "01.flac"),
		status("unassigned", "New", "/dl", "", "01.flac"),
		status("no-files", "Fetching", "/dl", ""),
		status("label-only", "Label", root+"/red", "stale", "movie.mkv"),
	}
	assignments := map[string]store.Assignment{
		"in-place":   {Hash: "in-place"},
		"placed":     {Hash: "placed", RuleID: audioID},
		"manual":     {Hash: "manual", Manual: true},
		"no-files":   {Hash: "no-files"},
		"label-only": {Hash: "label-only"},
	}

	var got []string
	for _, c := range Plan(statuses, assignments, rs, trackers, root) {
		got = append(got, c.Hash+" -> "+c.Placement.Dir+" "+c.Placement.Group)
	}
	require.Equal(t, []string{
		"in-place -> /data/torrents/red/audio 🎵 Audio",
		"manual -> /data/torrents/red/audio 🎵 Audio",
		"label-only -> /data/torrents/red ",
	}, got)
}

// logged points r's logger at a buffer, returning a function that drains the
// messages logged so far.
func logged(t *testing.T, r *Reconciler) func() []string {
	t.Helper()
	var buf bytes.Buffer
	r.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	return func() []string {
		defer buf.Reset()
		if buf.Len() == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(buf.String()), "\n")
	}
}

func TestReportLogsChanges(t *testing.T) {
	r := NewReconciler(nil, nil, root)
	drain := logged(t, r)
	down, other := errors.New("connection refused"), errors.New("database locked")

	r.report(nil)
	require.Empty(t, drain(), "a healthy start logs nothing")
	r.report(down)
	r.report(down)
	require.Len(t, drain(), 1, "a repeated failure logs once")
	r.report(other)
	require.Len(t, drain(), 1, "a different failure logs again")
	r.report(nil)
	got := drain()
	require.Len(t, got, 1)
	require.Contains(t, got[0], "reconcile recovered")
	r.report(nil)
	require.Empty(t, drain())
}

func TestObserve(t *testing.T) {
	r := NewReconciler(nil, nil, root)
	drain := logged(t, r)
	at := func(hash string, progress float64, errMsg string) torrent.Status {
		s := status(hash, "T"+hash, root, "")
		s.Progress, s.Error = progress, errMsg
		return s
	}

	r.observe([]torrent.Status{at("a", 0.5, ""), at("b", 1, ""), at("c", 0.2, "tracker gone")})
	got := drain()
	require.Len(t, got, 1, "first sighting logs existing errors, not completions")
	require.Contains(t, got[0], `level=WARN msg="torrent error" name=Tc`)

	r.observe([]torrent.Status{at("a", 1, ""), at("b", 1, "missing files"), at("c", 0.2, "")})
	got = drain()
	require.Len(t, got, 3)
	require.Contains(t, got[0], `msg="torrent completed" name=Ta`)
	require.Contains(t, got[1], `msg="torrent error" name=Tb`)
	require.Contains(t, got[2], `msg="torrent error cleared" name=Tc`)

	r.observe([]torrent.Status{at("a", 1, ""), at("b", 1, "missing files")})
	require.Empty(t, drain(), "unchanged torrents and removals log nothing")
}

func TestTiedTorrentLeftUnassigned(t *testing.T) {
	st, _ := setup(t, false)
	_, err := st.SaveRule(t.Context(), rules.Rule{
		Name: "🎧 Lossless", Color: "#007aff",
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}}},
	})
	require.NoError(t, err)

	album := status("new", "Album", "/home/me/Downloads", "", "01.flac")
	client := torrentmock.NewClient(t)
	client.EXPECT().Torrents(mock.Anything).Return([]torrent.Status{album}, nil).Twice()

	r := NewReconciler(client, st, root)
	drain := logged(t, r)
	require.NoError(t, r.Reconcile(t.Context()))
	require.NoError(t, r.Reconcile(t.Context()))
	require.Empty(t, assignments(t, st), "a tie is never placed automatically")
	got := drain()
	require.Len(t, got, 1, "the tie is logged once")
	require.Contains(t, got[0], `msg="torrent matches several rules equally; choose one in the UI"`)

	rs, err := st.Rules(t.Context())
	require.NoError(t, err)
	assigned := map[string]store.Assignment{"new": {Hash: "new"}}
	require.Empty(t, Plan([]torrent.Status{album}, assigned, rs, nil, root), "a tie never plans a move")
}
