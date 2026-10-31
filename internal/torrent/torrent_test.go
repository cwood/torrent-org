package torrent

import (
	"bytes"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/require"

	"github.com/cwood/torrent-org/internal/torrent/torrenttest"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		info     metainfo.Info
		announce metainfo.AnnounceList
		want     Torrent
	}{
		{
			name:     "single file",
			info:     metainfo.Info{Name: "movie.mkv", Length: 100},
			announce: metainfo.AnnounceList{{"https://tracker.example.org/announce?passkey=abc"}},
			want: Torrent{
				Name:     "movie.mkv",
				Trackers: []string{"https://tracker.example.org/announce?passkey=abc"},
				Files:    []File{{Path: "movie.mkv", Size: 100, Index: 0}},
			},
		},
		{
			name: "multi file skips padding",
			info: metainfo.Info{Name: "Album", Files: []metainfo.FileInfo{
				{Path: []string{"CD1", "01.flac"}, Length: 30},
				{Path: []string{".pad", "16354"}, Length: 16354, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
				{Path: []string{"cover.jpg"}, Length: 5},
			}},
			announce: metainfo.AnnounceList{{"udp://a.example:80"}, {"udp://b.example:80", "udp://a.example:80"}},
			want: Torrent{
				Name:     "Album",
				Trackers: []string{"udp://a.example:80", "udp://b.example:80"},
				Files: []File{
					{Path: "Album/CD1/01.flac", Size: 30, Index: 0},
					{Path: "Album/cover.jpg", Size: 5, Index: 2},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(bytes.NewReader(torrenttest.Encode(t, tt.info, tt.announce)))
			require.NoError(t, err)
			require.Len(t, got.Hash, 40)
			got.Hash = ""
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseInvalid(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte("not bencode")))
	require.Error(t, err)
}

func TestTrackerHosts(t *testing.T) {
	tor := Torrent{Trackers: []string{
		"https://Tracker.Example.org:443/announce",
		"udp://tracker.example.org:6969",
		"::not a url",
		"http://other.example/announce",
	}}
	require.Equal(t, []string{"tracker.example.org", "other.example"}, tor.TrackerHosts())
}

func TestDominantExtension(t *testing.T) {
	tests := []struct {
		name  string
		files []File
		want  string
	}{
		{name: "empty", want: ""},
		{name: "no extensions", files: []File{{Path: "README", Size: 10}}, want: ""},
		{
			name: "largest by bytes not count",
			files: []File{
				{Path: "a/sample.mp4", Size: 50},
				{Path: "a/01.FLAC", Size: 40},
				{Path: "a/02.flac", Size: 40},
				{Path: "a/info.nfo", Size: 1},
			},
			want: "flac",
		},
		{
			name:  "tie breaks alphabetically",
			files: []File{{Path: "b.mkv", Size: 10}, {Path: "a.avi", Size: 10}},
			want:  "avi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Torrent{Files: tt.files}.DominantExtension())
		})
	}
}

func TestDataPath(t *testing.T) {
	status := func(name string, files ...string) Status {
		s := Status{Torrent: Torrent{Name: name}, Dir: "/data/torrents/red"}
		for _, f := range files {
			s.Files = append(s.Files, File{Path: f})
		}
		return s
	}
	require.Equal(t, "/data/torrents/red/Album", status("Album", "Album/01.flac", "Album/cover.jpg").DataPath())
	require.Equal(t, "/data/torrents/red/movie.mkv", status("movie.mkv", "movie.mkv").DataPath())
	require.Equal(t, "/data/torrents/red/Magnet", status("Magnet").DataPath(), "no metadata yet")
}
