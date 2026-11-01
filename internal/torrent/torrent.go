// Package torrent models the parts of a torrent that placement decisions
// depend on, independent of where the data came from (a .torrent file or the
// Transmission RPC).
package torrent

import (
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// File is a single file inside a torrent. Path is slash-separated and
// includes the torrent's top-level directory for multi-file torrents.
//
// Index is the file's position in the torrent's v1 file list, which is how
// clients address files when choosing what to download. BEP 47 padding files
// occupy positions there but are omitted from Torrent.Files, so indexes can
// have gaps.
type File struct {
	Path  string
	Size  int64
	Index int
	// Wanted and Done are known only for torrents a client holds: whether
	// the file is selected for download and how many of its bytes are.
	Wanted bool
	Done   int64
}

// Progress is the fraction of f downloaded, from 0 to 1.
func (f File) Progress() float64 {
	if f.Size == 0 {
		return 1
	}
	return float64(f.Done) / float64(f.Size)
}

// Torrent is the metadata needed to classify and place a torrent.
type Torrent struct {
	Hash     string
	Name     string
	Trackers []string
	Files    []File
}

// Parse reads a bencoded .torrent file.
func Parse(r io.Reader) (Torrent, error) {
	mi, err := metainfo.Load(r)
	if err != nil {
		return Torrent{}, fmt.Errorf("decode metainfo: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return Torrent{}, fmt.Errorf("decode info dict: %w", err)
	}

	t := Torrent{
		Hash:     mi.HashInfoBytes().HexString(),
		Name:     info.BestName(),
		Trackers: mi.UpvertedAnnounceList().DistinctValues(),
	}
	// Transmission indexes files by the v1 list, padding included, so walk
	// that rather than the v2 file tree.
	i := 0
	for fi := range info.UpvertedV1Files() {
		i++
		// Padding files exist only for piece alignment and would skew
		// DominantExtension towards "".
		if strings.Contains(fi.Attr, "p") {
			continue
		}
		p := info.BestName()
		if info.IsDir() {
			p = path.Join(append([]string{p}, fi.BestPath()...)...)
		}
		t.Files = append(t.Files, File{Path: p, Size: fi.Length, Index: i - 1})
	}
	return t, nil
}

// TrackerHosts returns the distinct hostnames of the torrent's announce URLs
// in announce-list order. Unparseable URLs are skipped.
func (t Torrent) TrackerHosts() []string {
	var hosts []string
	for _, raw := range t.Trackers {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// DominantExtension returns the lowercase extension, without the leading dot,
// that accounts for the most bytes in the torrent. Weighting by size keeps a
// stray .nfo or sample clip from deciding what an album or season is. It
// returns "" when no file has an extension.
func (t Torrent) DominantExtension() string {
	bytes := make(map[string]int64)
	for _, f := range t.Files {
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(f.Path), "."))
		if ext == "" {
			continue
		}
		bytes[ext] += f.Size
	}

	var best string
	for ext, n := range bytes {
		// Break ties alphabetically so the result is deterministic.
		if n > bytes[best] || (n == bytes[best] && ext < best) {
			best = ext
		}
	}
	return best
}
