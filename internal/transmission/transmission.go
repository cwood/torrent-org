// Package transmission implements [torrent.Client] over the Transmission RPC.
// Groups are stored as the torrent's sole label.
package transmission

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/hekmon/transmissionrpc/v3"

	"github.com/cwood/torrent-org/internal/config"
	"github.com/cwood/torrent-org/internal/torrent"
)

// minRPCVersion is the first RPC version with torrent labels (Transmission
// 4.0).
const minRPCVersion = 16

var fields = []string{
	"hashString", "name", "downloadDir", "labels", "files", "wanted", "trackers",
	"percentDone", "rateDownload", "rateUpload", "eta", "status", "errorString",
}

var _ torrent.Client = (*Client)(nil)

// Client talks to a Transmission daemon.
type Client struct {
	rpc *transmissionrpc.Client
}

// ErrIncompatible is returned by [Client.Check] when the server's RPC version
// cannot be used, as opposed to the server being unreachable.
var ErrIncompatible = errors.New("incompatible transmission rpc version")

// New creates a client for the configured RPC endpoint without contacting
// it, so the service can start before Transmission does.
func New(cfg config.Transmission) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse transmission url: %w", err)
	}
	// net/http sends basic auth from the URL's userinfo.
	if cfg.Username != "" {
		u.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	rpc, err := transmissionrpc.New(u, &transmissionrpc.Config{UserAgent: "torrent-org"})
	if err != nil {
		return nil, fmt.Errorf("create transmission client: %w", err)
	}
	return &Client{rpc: rpc}, nil
}

// Check verifies the server is reachable and speaks an RPC version with label
// support that the client library is compatible with. Version problems wrap
// [ErrIncompatible].
func (c *Client) Check(ctx context.Context) error {
	ok, version, minimum, err := c.rpc.RPCVersion(ctx)
	if err != nil {
		return fmt.Errorf("query transmission rpc version: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: server requires %d, client supports %d", ErrIncompatible, minimum, transmissionrpc.RPCVersion)
	}
	if version < minRPCVersion {
		return fmt.Errorf("%w: version %d lacks labels; need %d (Transmission 4.0+)", ErrIncompatible, version, minRPCVersion)
	}
	return nil
}

// WebURL is the address of the web interface Transmission serves beside the
// RPC endpoint rpcURL. Credentials are dropped so it is safe to put in a
// page; the web interface asks for them itself.
func WebURL(rpcURL string) (string, error) {
	u, err := url.Parse(rpcURL)
	if err != nil {
		return "", fmt.Errorf("parse transmission url: %w", err)
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.Path = path.Join(path.Dir(u.Path), "web") + "/"
	return u.String(), nil
}

// Torrents lists every torrent in the daemon.
func (c *Client) Torrents(ctx context.Context) ([]torrent.Status, error) {
	rows, err := c.rpc.TorrentGet(ctx, fields, nil)
	if err != nil {
		return nil, err
	}
	out := make([]torrent.Status, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRPC(r))
	}
	return out, nil
}

func fromRPC(r transmissionrpc.Torrent) torrent.Status {
	s := torrent.Status{
		Torrent: torrent.Torrent{
			Hash: value(r.HashString),
			Name: value(r.Name),
		},
		Dir:      value(r.DownloadDir),
		Progress: value(r.PercentDone),
		RateDown: value(r.RateDownload),
		RateUp:   value(r.RateUpload),
		State:    state(value(r.Status)),
		Error:    value(r.ErrorString),
	}
	// eta is negative when Transmission has no estimate. Once complete it
	// counts down to a seed ratio limit instead, which isn't a download
	// estimate.
	if eta := value(r.ETA); eta > 0 && s.Progress < 1 {
		s.ETA = time.Duration(eta) * time.Second
	}
	// SetGroup keeps a single label, so the first one is the group.
	if len(r.Labels) > 0 {
		s.Group = r.Labels[0]
	}
	for _, tr := range r.Trackers {
		s.Trackers = append(s.Trackers, tr.Announce)
	}
	// Transmission file names already include the torrent's top-level
	// directory, matching torrent.Parse.
	for i, f := range r.Files {
		s.Files = append(s.Files, torrent.File{
			Path: f.Name, Size: f.Length, Index: i,
			Wanted: i < len(r.Wanted) && r.Wanted[i], Done: f.BytesCompleted,
		})
	}
	return s
}

func state(s transmissionrpc.TorrentStatus) torrent.State {
	switch s {
	case transmissionrpc.TorrentStatusCheckWait, transmissionrpc.TorrentStatusCheck:
		return torrent.StateChecking
	case transmissionrpc.TorrentStatusDownloadWait, transmissionrpc.TorrentStatusSeedWait:
		return torrent.StateQueued
	case transmissionrpc.TorrentStatusDownload:
		return torrent.StateDownloading
	case transmissionrpc.TorrentStatusSeed:
		return torrent.StateSeeding
	}
	return torrent.StateStopped
}

// value dereferences RPC fields, which the library models as pointers so it
// can tell "not requested" from zero.
func value[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// labels is the label set representing group. It is never nil: the library
// omits nil slices from payloads, which would leave labels unchanged instead
// of clearing them.
func labels(group string) []string {
	if group == "" {
		return []string{}
	}
	return []string{group}
}

// Add adds a torrent labelled with its group.
func (c *Client) Add(ctx context.Context, a torrent.Addition) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(a.Metainfo)
	payload := transmissionrpc.TorrentAddPayload{
		MetaInfo:    &b64,
		DownloadDir: &a.Dir,
		Labels:      labels(a.Group),
	}
	if len(a.Skip) > 0 {
		payload.FilesUnwanted = indexes(a.Skip)
	}
	res, err := c.rpc.TorrentAdd(ctx, payload)
	if err != nil {
		return "", err
	}
	if res.HashString == nil {
		return "", errors.New("torrent-add response has no hash")
	}
	return *res.HashString, nil
}

// Move stops the torrent if it is active, moves its data to dir, verifies
// the moved data and starts it again, so nothing writes to the files mid-move
// and a bad move is caught before seeding resumes. A stopped torrent is
// verified but stays stopped.
func (c *Client) Move(ctx context.Context, hash, dir string) (err error) {
	t, err := c.lookup(ctx, hash, "id", "status")
	if err != nil {
		return err
	}
	ids := []int64{*t.ID}

	if value(t.Status) != transmissionrpc.TorrentStatusStopped {
		if err := c.rpc.TorrentStopIDs(ctx, ids); err != nil {
			return err
		}
		// Restart even if a later step fails so the torrent isn't left
		// stopped. Transmission runs RPC actions on its session thread in
		// order and holds a start until verification completes.
		defer func() { err = errors.Join(err, c.rpc.TorrentStartIDs(ctx, ids)) }()
	}
	if err := c.rpc.TorrentSetLocation(ctx, ids[0], dir, true); err != nil {
		return err
	}
	return c.rpc.TorrentVerifyIDs(ctx, ids)
}

// SetGroup replaces the torrent's labels with group, or clears them when
// group is empty.
func (c *Client) SetGroup(ctx context.Context, hash, group string) error {
	t, err := c.lookup(ctx, hash, "id")
	if err != nil {
		return err
	}
	return c.rpc.TorrentSet(ctx, transmissionrpc.TorrentSetPayload{IDs: []int64{*t.ID}, Labels: labels(group)})
}

// SetWanted changes which of the torrent's files are downloaded.
func (c *Client) SetWanted(ctx context.Context, hash string, wanted, unwanted []int) error {
	t, err := c.lookup(ctx, hash, "id")
	if err != nil {
		return err
	}
	return c.rpc.TorrentSet(ctx, transmissionrpc.TorrentSetPayload{
		IDs: []int64{*t.ID}, FilesWanted: indexes(wanted), FilesUnwanted: indexes(unwanted),
	})
}

func indexes(is []int) []int64 {
	out := make([]int64, 0, len(is))
	for _, i := range is {
		out = append(out, int64(i))
	}
	return out
}

// lookup fetches fields, which must include "id", for the torrent with hash.
// Mutations need Transmission's session-scoped numeric ID because the library
// only accepts IDs for them.
func (c *Client) lookup(ctx context.Context, hash string, fields ...string) (transmissionrpc.Torrent, error) {
	rows, err := c.rpc.TorrentGetHashes(ctx, fields, []string{hash})
	if err != nil {
		return transmissionrpc.Torrent{}, err
	}
	if len(rows) == 0 || rows[0].ID == nil {
		return transmissionrpc.Torrent{}, fmt.Errorf("%s: %w", hash, torrent.ErrNotFound)
	}
	return rows[0], nil
}
