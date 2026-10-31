package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/rules"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestRules(t *testing.T) {
	ctx := t.Context()
	s := open(t, filepath.Join(t.TempDir(), "nested", "test.db"))

	audio := rules.Rule{
		Name: "🎵 Audio", Color: "#ff8800", Priority: 2, Mode: rules.ModeAll,
		Path:       "{root}/{tracker}/audio",
		Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "flac"}},
	}
	video := rules.Rule{
		Name: "Video", Color: "#4f8cff", Priority: 1, Mode: rules.ModeAny,
		Conditions: []rules.Condition{
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mkv"},
			{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mp4"},
		},
	}
	var err error
	audio.ID, err = s.SaveRule(ctx, audio)
	require.NoError(t, err)
	video.ID, err = s.SaveRule(ctx, video)
	require.NoError(t, err)

	got, err := s.Rules(ctx)
	require.NoError(t, err)
	require.Equal(t, []rules.Rule{video, audio}, got)

	audio.Priority = 0
	audio.Conditions = []rules.Condition{{Field: rules.FieldName, Op: rules.OpContains, Value: "flac"}}
	_, err = s.SaveRule(ctx, audio)
	require.NoError(t, err)
	got, err = s.Rules(ctx)
	require.NoError(t, err)
	require.Equal(t, []rules.Rule{audio, video}, got)

	_, err = s.SaveRule(ctx, rules.Rule{ID: 999, Name: "Ghost", Color: "#000", Mode: rules.ModeAll})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.SaveRule(ctx, rules.Rule{Name: "🎵 Audio", Color: "#000", Mode: rules.ModeAll})
	require.Error(t, err, "rule names are unique")

	_, err = s.SaveRule(ctx, rules.Rule{Name: "Bad", Color: "#000", Mode: rules.Mode("some")})
	require.Error(t, err, "mode is constrained")
}

func TestDeleteRuleKeepsAssignment(t *testing.T) {
	ctx := t.Context()
	s := open(t, filepath.Join(t.TempDir(), "test.db"))

	id, err := s.SaveRule(ctx, rules.Rule{
		Name: "Video", Color: "#4f8cff", Mode: rules.ModeAll,
		Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs, Value: "mkv"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.Assign(ctx, Assignment{Hash: "abc", RuleID: id, Manual: true}))
	require.NoError(t, s.Assign(ctx, Assignment{Hash: "def"}))

	require.NoError(t, s.DeleteRule(ctx, id))

	got, err := s.Rules(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
	assignments, err := s.Assignments(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]Assignment{
		"abc": {Hash: "abc", Manual: true},
		"def": {Hash: "def"},
	}, assignments)

	require.NoError(t, s.Unassign(ctx, "abc"))
	assignments, err = s.Assignments(ctx)
	require.NoError(t, err)
	require.Len(t, assignments, 1)
}

func TestAssignUnknownRule(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "test.db"))
	require.Error(t, s.Assign(t.Context(), Assignment{Hash: "abc", RuleID: 42}), "foreign keys are enforced")
}

func TestTrackers(t *testing.T) {
	ctx := t.Context()
	s := open(t, filepath.Join(t.TempDir(), "test.db"))

	require.NoError(t, s.SetTracker(ctx, "flacsfor.me", "red"))
	require.NoError(t, s.SetTracker(ctx, "tracker.example.org", "example"))
	require.NoError(t, s.SetTracker(ctx, "flacsfor.me", "redacted"))
	require.NoError(t, s.DeleteTracker(ctx, "tracker.example.org"))

	got, err := s.Trackers(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"flacsfor.me": "redacted"}, got)
}

func TestReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, s.SetTracker(t.Context(), "flacsfor.me", "red"))
	require.NoError(t, s.Close())

	got, err := open(t, path).Trackers(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"flacsfor.me": "red"}, got)
}

func TestBaseline(t *testing.T) {
	ctx := t.Context()
	s := open(t, filepath.Join(t.TempDir(), "test.db"))

	ok, err := s.HasBaseline(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.RecordBaseline(ctx, []string{"abc", "def"}))
	require.NoError(t, s.RecordBaseline(ctx, nil), "recording twice is harmless")

	ok, err = s.HasBaseline(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assignments, err := s.Assignments(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]Assignment{"abc": {Hash: "abc"}, "def": {Hash: "def"}}, assignments)
}

func TestReorderRules(t *testing.T) {
	ctx := t.Context()
	s := open(t, filepath.Join(t.TempDir(), "test.db"))

	var ids []int64
	for i, name := range []string{"a", "b", "c"} {
		id, err := s.SaveRule(ctx, rules.Rule{Name: name, Color: "#000000", Priority: int64(i), Mode: rules.ModeAll})
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, s.ReorderRules(ctx, []int64{ids[2], ids[0], ids[1]}))

	got, err := s.Rules(ctx)
	require.NoError(t, err)
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	require.Equal(t, []string{"c", "a", "b"}, names)
}
