package torrent

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"
)

// ErrNotFound is returned when a client does not hold the requested torrent.
var ErrNotFound = errors.New("torrent not found")

//go:generate go tool mockery

// Client is a torrent client that holds torrents and places them on disk.
// Implementations identify torrents by info hash and map groups onto
// whatever the client offers (labels, categories, ...).
type Client interface {
	// Torrents lists every torrent the client holds.
	Torrents(ctx context.Context) ([]Status, error)
	// Add adds a torrent and returns its info hash. If the client already
	// holds the torrent it is left untouched.
	Add(ctx context.Context, a Addition) (string, error)
	// Move moves a torrent's data to dir, pausing an active torrent for the
	// duration and resuming it afterwards.
	Move(ctx context.Context, hash, dir string) error
	// SetGroup assigns a torrent to group. An empty group clears it.
	SetGroup(ctx context.Context, hash, group string) error
}

// Addition is a torrent to add to a client.
type Addition struct {
	// Metainfo is the .torrent file's contents.
	Metainfo []byte
	Dir      string
	// Group is the group to assign; empty assigns none.
	Group string
	// Skip lists the [File.Index] of files not to download.
	Skip []int
}

// State is what a client is currently doing with a torrent.
type State string

const (
	StateStopped     State = "stopped"
	StateQueued      State = "queued"
	StateChecking    State = "checking"
	StateDownloading State = "downloading"
	StateSeeding     State = "seeding"
)

// Status is a torrent as a client currently holds it.
type Status struct {
	Torrent
	Dir   string
	Group string
	// Progress is the fraction of wanted files downloaded, from 0 to 1.
	Progress float64
	RateDown int64
	RateUp   int64
	// ETA is the estimated time left to download; zero when complete or
	// when the client has no estimate.
	ETA   time.Duration
	State State
	Error string
}

// DataPath is the torrent's top-level file or directory on disk: the single
// file, or the directory holding a multi-file torrent's files.
func (s Status) DataPath() string {
	if len(s.Files) == 0 {
		return path.Join(s.Dir, s.Name)
	}
	top, _, _ := strings.Cut(s.Files[0].Path, "/")
	return path.Join(s.Dir, top)
}

// Complete reports whether every wanted file has finished downloading.
func (s Status) Complete() bool {
	return s.Progress >= 1
}
