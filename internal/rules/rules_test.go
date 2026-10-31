package rules

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/torrent"
)

var (
	show = torrent.Torrent{
		Name:     "Some.Show.S01E02.1080p.WEB",
		Trackers: []string{"https://tracker.example.org/announce"},
		Files: []torrent.File{
			{Path: "Some.Show.S01E02.1080p.WEB/episode.mkv", Size: 1000},
			{Path: "Some.Show.S01E02.1080p.WEB/info.nfo", Size: 1},
		},
	}
	album = torrent.Torrent{
		Name:     "Artist - Album (2024) [FLAC]",
		Trackers: []string{"https://flacsfor.me/abc/announce", "udp://backup.example:80"},
		Files: []torrent.File{
			{Path: "Artist - Album/01.flac", Size: 300},
			{Path: "Artist - Album/cover.jpg", Size: 2},
		},
	}
)

func TestRuleMatches(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		t    torrent.Torrent
		want bool
	}{
		{
			name: "no conditions never matches",
			rule: Rule{Mode: ModeAll},
			t:    show,
		},
		{
			name: "extension is, dot and case ignored",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldExtension, OpIs, ".MKV"}}},
			t:    show,
			want: true,
		},
		{
			name: "extension uses dominant file not any file",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldExtension, OpIs, "nfo"}}},
			t:    show,
		},
		{
			name: "name contains",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldName, OpContains, "s01e"}}},
			t:    show,
			want: true,
		},
		{
			name: "name begins with",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldName, OpBeginsWith, "artist"}}},
			t:    album,
			want: true,
		},
		{
			name: "name ends with",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldName, OpEndsWith, "[flac]"}}},
			t:    album,
			want: true,
		},
		{
			name: "tracker matches any host",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldTracker, OpIs, "backup.example"}}},
			t:    album,
			want: true,
		},
		{
			name: "all requires every condition",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{
				{FieldExtension, OpIs, "flac"},
				{FieldTracker, OpContains, "example.org"},
			}},
			t: album,
		},
		{
			name: "any requires one condition",
			rule: Rule{Mode: ModeAny, Conditions: []Condition{
				{FieldExtension, OpIs, "flac"},
				{FieldTracker, OpContains, "example.org"},
			}},
			t:    album,
			want: true,
		},
		{
			name: "unknown op never matches",
			rule: Rule{Mode: ModeAll, Conditions: []Condition{{FieldName, Op("regex"), ".*"}}},
			t:    album,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.rule.Matches(tt.t))
		})
	}
}

func TestPlace(t *testing.T) {
	video := Rule{
		ID: 1, Name: "Video", Priority: 1, Mode: ModeAny,
		Path:       "{root}/{tracker}/video",
		Conditions: []Condition{{FieldExtension, OpIs, "mkv"}, {FieldExtension, OpIs, "mp4"}},
	}
	audio := Rule{
		ID: 2, Name: "Audio", Priority: 2, Mode: ModeAll,
		Path:       "{root}/{tracker}/audio/",
		Conditions: []Condition{{FieldExtension, OpIs, "flac"}},
	}
	everything := Rule{
		ID: 3, Name: "Everything", Priority: 3, Mode: ModeAll,
		Conditions: []Condition{{FieldName, OpContains, ""}},
	}
	trackers := map[string]string{"flacsfor.me": "red", "backup.example": "backup"}

	tests := []struct {
		name  string
		t     torrent.Torrent
		rules []Rule
		want  Placement
	}{
		{
			name:  "first matching rule wins",
			t:     show,
			rules: []Rule{video, audio, everything},
			want: Placement{
				Rule: &video, Tracker: "tracker.example.org",
				Dir: "/data/torrents/tracker.example.org/video", Group: "Video",
			},
		},
		{
			name:  "mapped tracker name and cleaned path",
			t:     album,
			rules: []Rule{video, audio},
			want: Placement{
				Rule: &audio, Tracker: "red",
				Dir: "/data/torrents/red/audio", Group: "Audio",
			},
		},
		{
			name:  "rule without path uses default location",
			t:     album,
			rules: []Rule{everything},
			want: Placement{
				Rule: &everything, Tracker: "red",
				Dir: "/data/torrents/red", Group: "Everything",
			},
		},
		{
			name:  "no match",
			t:     torrent.Torrent{Name: "x"},
			rules: []Rule{video, audio},
			want:  Placement{Tracker: Untracked, Dir: "/data/torrents/untracked"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Place(tt.t, tt.rules, trackers, "/data/torrents"))
		})
	}
}

func TestValidate(t *testing.T) {
	valid := Rule{
		Name: "🎵 Audio", Color: "#ff8800", Mode: ModeAll, Path: "{root}/{tracker}/audio",
		Conditions: []Condition{{FieldExtension, OpIs, "flac"}},
	}
	tests := []struct {
		name    string
		mutate  func(*Rule)
		wantErr string
	}{
		{name: "valid", mutate: func(*Rule) {}},
		{name: "no conditions is manual only", mutate: func(r *Rule) { r.Conditions = nil }},
		{name: "empty path uses default", mutate: func(r *Rule) { r.Path = "" }},
		{name: "absolute path", mutate: func(r *Rule) { r.Path = "/srv/audio" }},
		{name: "blank name", mutate: func(r *Rule) { r.Name = "  " }, wantErr: "name is required"},
		{name: "comma in name", mutate: func(r *Rule) { r.Name = "a,b" }, wantErr: "commas"},
		{name: "bad color", mutate: func(r *Rule) { r.Color = "red" }, wantErr: "color"},
		{name: "bad mode", mutate: func(r *Rule) { r.Mode = "some" }, wantErr: "mode"},
		{name: "relative path", mutate: func(r *Rule) { r.Path = "audio" }, wantErr: "path"},
		{name: "bad field", mutate: func(r *Rule) { r.Conditions[0].Field = "size" }, wantErr: "unknown field"},
		{name: "bad op", mutate: func(r *Rule) { r.Conditions[0].Op = "regex" }, wantErr: "unknown comparison"},
		{name: "empty value", mutate: func(r *Rule) { r.Conditions[0].Value = "" }, wantErr: "value is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			r.Conditions = slices.Clone(valid.Conditions)
			tt.mutate(&r)
			err := r.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
