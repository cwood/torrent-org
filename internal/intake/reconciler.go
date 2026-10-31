// Package intake brings torrents under management: torrents that appear in
// the client are placed by the rules and recorded in the store.
package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

// Reconciler places torrents that have no assignment yet. A torrent whose
// most specific rules tie is left unassigned for the user to settle.
//
// The first pass ever records every existing torrent in place instead of
// moving it. After that, any unassigned torrent is new, including ones added
// while the service was down, and is moved and grouped by the rules.
// Assigned torrents are never re-evaluated.
type Reconciler struct {
	client torrent.Client
	store  *store.Store
	root   string
	log    *slog.Logger
	// mu serializes passes with Add so a pass never places a torrent that
	// Add is about to assign. It also guards seen and tied.
	mu   sync.Mutex
	seen map[string]seen
	// tied holds unassigned torrents whose best rules tie, so each is
	// logged once rather than every pass.
	tied map[string]bool
	// failure is the previous pass's error text; only Run touches it.
	failure string
}

// seen is what a pass last observed of a torrent, so the next pass can log
// what changed.
type seen struct {
	complete bool
	err      string
}

// ErrDuplicate is returned by Add when the client already holds the torrent.
var ErrDuplicate = errors.New("torrent is already in the client")

// NewReconciler creates a Reconciler placing torrents under root, as the
// client sees it.
func NewReconciler(client torrent.Client, st *store.Store, root string) *Reconciler {
	return &Reconciler{client: client, store: st, root: root, log: slog.Default()}
}

// Run reconciles immediately and then every interval until ctx is done.
// Failed passes are retried on the next tick.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); ctx.Err() == nil {
			r.report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// report logs a pass's outcome only when it differs from the previous pass,
// so an unreachable client logs once instead of every tick.
func (r *Reconciler) report(err error) {
	var msg string
	if err != nil {
		msg = err.Error()
	}
	switch {
	case msg == r.failure:
	case err != nil:
		r.log.Error("reconcile failed; retrying", "err", err)
	default:
		r.log.Info("reconcile recovered")
	}
	r.failure = msg
}

// observe logs torrents completing and torrent errors appearing, changing or
// clearing. A torrent's first sighting logs only an existing error, so a
// restart doesn't announce every finished torrent again.
func (r *Reconciler) observe(statuses []torrent.Status) {
	next := make(map[string]seen, len(statuses))
	for _, s := range statuses {
		now := seen{complete: s.Complete(), err: s.Error}
		prev, known := r.seen[s.Hash]
		next[s.Hash] = now
		if known && now.complete && !prev.complete {
			r.log.Info("torrent completed", "name", s.Name, "hash", s.Hash, "dir", s.Dir)
		}
		switch {
		case now.err == prev.err:
		case now.err != "":
			r.log.Warn("torrent error", "name", s.Name, "hash", s.Hash, "err", now.err)
		default:
			r.log.Info("torrent error cleared", "name", s.Name, "hash", s.Hash)
		}
	}
	r.seen = next
}

// Reconcile runs a single pass. A torrent that fails to place is left
// unassigned so the next pass retries it; the others still proceed.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	statuses, err := r.client.Torrents(ctx)
	if err != nil {
		return fmt.Errorf("list torrents: %w", err)
	}
	r.observe(statuses)
	assignments, err := r.store.Assignments(ctx)
	if err != nil {
		return err
	}

	var unassigned []torrent.Status
	held := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		held[s.Hash] = true
		// Torrents added by magnet have no files until metadata arrives;
		// placing them now would ignore any extension rule.
		if _, ok := assignments[s.Hash]; !ok && len(s.Files) > 0 {
			unassigned = append(unassigned, s)
		}
	}
	return errors.Join(r.forget(ctx, assignments, held), r.placeAll(ctx, unassigned))
}

// forget drops assignments for torrents the client no longer holds.
func (r *Reconciler) forget(ctx context.Context, assignments map[string]store.Assignment, held map[string]bool) error {
	var errs []error
	for hash := range assignments {
		if held[hash] {
			continue
		}
		if err := r.store.Unassign(ctx, hash); err != nil {
			errs = append(errs, fmt.Errorf("forget %s: %w", hash, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) placeAll(ctx context.Context, unassigned []torrent.Status) error {
	baselined, err := r.store.HasBaseline(ctx)
	if err != nil {
		return err
	}
	if !baselined {
		hashes := make([]string, 0, len(unassigned))
		for _, s := range unassigned {
			hashes = append(hashes, s.Hash)
		}
		if err := r.store.RecordBaseline(ctx, hashes); err != nil {
			return err
		}
		r.log.Info("recorded existing torrents in place", "count", len(hashes))
		return nil
	}
	if len(unassigned) == 0 {
		return nil
	}

	rs, err := r.store.Rules(ctx)
	if err != nil {
		return err
	}
	trackers, err := r.store.Trackers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	tied := make(map[string]bool)
	for _, s := range unassigned {
		p := rules.Place(s.Torrent, rs, trackers, r.root)
		if len(p.Ties) > 0 {
			if !r.tied[s.Hash] {
				r.log.Warn("torrent matches several rules equally; choose one in the UI", "name", s.Name, "hash", s.Hash, "rules", ruleNames(p.Ties))
			}
			tied[s.Hash] = true
			continue
		}
		if err := r.place(ctx, s, p, false); err != nil {
			errs = append(errs, fmt.Errorf("place %s (%s): %w", s.Name, s.Hash, err))
		}
	}
	r.tied = tied
	return errors.Join(errs...)
}

func ruleNames(rs []rules.Rule) []string {
	names := make([]string, 0, len(rs))
	for _, rule := range rs {
		names = append(names, rule.Name)
	}
	return names
}

// place moves and regroups s where needed to match p and records the
// assignment.
func (r *Reconciler) place(ctx context.Context, s torrent.Status, p rules.Placement, manual bool) error {
	if s.Dir != p.Dir {
		if err := r.client.Move(ctx, s.Hash, p.Dir); err != nil {
			return fmt.Errorf("move to %s: %w", p.Dir, err)
		}
	}
	if s.Group != p.Group {
		if err := r.client.SetGroup(ctx, s.Hash, p.Group); err != nil {
			return fmt.Errorf("set group %q: %w", p.Group, err)
		}
	}
	if err := r.assign(ctx, s.Hash, p, manual); err != nil {
		return err
	}
	r.log.Info("placed torrent", "name", s.Name, "hash", s.Hash, "dir", p.Dir, "group", p.Group, "manual", manual)
	return nil
}

func (r *Reconciler) assign(ctx context.Context, hash string, p rules.Placement, manual bool) error {
	a := store.Assignment{Hash: hash, Manual: manual}
	if p.Rule != nil {
		a.RuleID = p.Rule.ID
	}
	return r.store.Assign(ctx, a)
}

// Addition is a .torrent to add to the client and where to place it.
type Addition struct {
	Metainfo  []byte
	Hash      string
	Placement rules.Placement
	// Skip lists the torrent.File.Index of files not to download.
	Skip []int
	// Manual marks a placement the user chose over the rules' pick.
	Manual bool
}

// Add adds a torrent to the client and records its assignment.
func (r *Reconciler) Add(ctx context.Context, a Addition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	statuses, err := r.client.Torrents(ctx)
	if err != nil {
		return fmt.Errorf("list torrents: %w", err)
	}
	if slices.ContainsFunc(statuses, func(s torrent.Status) bool { return s.Hash == a.Hash }) {
		return ErrDuplicate
	}
	p := a.Placement
	if _, err := r.client.Add(ctx, torrent.Addition{Metainfo: a.Metainfo, Dir: p.Dir, Group: p.Group, Skip: a.Skip}); err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	if err := r.assign(ctx, a.Hash, p, a.Manual); err != nil {
		return err
	}
	r.log.Info("added torrent", "hash", a.Hash, "dir", p.Dir, "group", p.Group, "skipped", len(a.Skip), "manual", a.Manual)
	return nil
}

// Move is a placement to apply to a torrent the client already holds.
type Move struct {
	Hash      string
	Placement rules.Placement
	// Manual marks a placement the user chose over the rules' pick.
	Manual bool
}

// Relocate applies moves to torrents already in the client, recording each
// as the torrent's assignment. A move that fails is skipped and reported;
// the rest proceed.
func (r *Reconciler) Relocate(ctx context.Context, moves []Move) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	statuses, err := r.client.Torrents(ctx)
	if err != nil {
		return fmt.Errorf("list torrents: %w", err)
	}
	var errs []error
	for _, m := range moves {
		i := slices.IndexFunc(statuses, func(s torrent.Status) bool { return s.Hash == m.Hash })
		if i < 0 {
			errs = append(errs, fmt.Errorf("%s: %w", m.Hash, torrent.ErrNotFound))
			continue
		}
		s := statuses[i]
		if err := r.place(ctx, s, m.Placement, m.Manual); err != nil {
			errs = append(errs, fmt.Errorf("relocate %s: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Change is an assigned torrent whose directory or group differs from where
// the rules would now place it.
type Change struct {
	torrent.Status
	Assignment store.Assignment
	Placement  rules.Placement
}

// Plan returns the torrents rs would place somewhere other than where they
// are. Torrents without an assignment are left to the reconciler, and
// torrents whose best rules tie stay put until the user picks one. Manual
// assignments are included so callers can show them, but their placement
// was chosen by the user and must not be applied.
func Plan(statuses []torrent.Status, assignments map[string]store.Assignment, rs []rules.Rule, trackers map[string]string, root string) []Change {
	var out []Change
	for _, s := range statuses {
		a, ok := assignments[s.Hash]
		if !ok || len(s.Files) == 0 {
			continue
		}
		p := rules.Place(s.Torrent, rs, trackers, root)
		if len(p.Ties) > 0 {
			continue
		}
		if p.Dir != s.Dir || p.Group != s.Group {
			out = append(out, Change{Status: s, Assignment: a, Placement: p})
		}
	}
	return out
}
