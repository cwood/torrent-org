package web

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

// unlockHeader carries a page's unlock key on htmx requests. EventSource
// can't set headers, so the event stream takes it as the "key" query value.
const unlockHeader = "X-Unlock-Key"

const (
	minPasscode = 4
	// maxPasscode is the most bcrypt accepts.
	maxPasscode = 72
	maxBackoff  = 30 * time.Second
)

// unlocks tracks the keys of pages showing private rules, and slows down
// passcode guesses. Keys live in the page's memory, not a cookie, so a reload
// or new tab starts hidden. Here they last until locked or the service
// restarts.
type unlocks struct {
	mu      sync.Mutex
	keys    map[string]bool
	fails   int
	retryAt time.Time
	now     func() time.Time
}

func newUnlocks() *unlocks {
	return &unlocks{keys: make(map[string]bool), now: time.Now}
}

func (u *unlocks) issue() string {
	key := rand.Text()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.keys[key] = true
	u.fails, u.retryAt = 0, time.Time{}
	return key
}

func (u *unlocks) revoke(key string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.keys, key)
}

func (u *unlocks) valid(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.keys[key]
}

// waiting reports whether a recent wrong passcode still blocks guesses.
func (u *unlocks) waiting() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.now().Before(u.retryAt)
}

// failed doubles the wait before the next guess, up to maxBackoff.
func (u *unlocks) failed() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.retryAt = u.now().Add(min(time.Second<<min(u.fails, 8), maxBackoff))
	u.fails++
}

func requestKey(r *http.Request) string {
	if k := r.Header.Get(unlockHeader); k != "" {
		return k
	}
	return r.URL.Query().Get("key")
}

// concealing reports whether r comes from a page that hasn't been unlocked.
func (s *Server) concealing(r *http.Request) bool {
	return !s.unlocks.valid(requestKey(r))
}

// privateTorrent reports whether showing t would reveal a private rule: it is
// assigned to one, or isn't pinned and a private rule is among its best
// matches. Ties count, since the torrent could end up in that rule.
func privateTorrent(t torrent.Torrent, a store.Assignment, rs []rules.Rule) bool {
	if slices.ContainsFunc(rs, func(r rules.Rule) bool { return r.Private && r.ID == a.RuleID }) {
		return true
	}
	return !a.Manual && slices.ContainsFunc(rules.Match(t, rs), func(r rules.Rule) bool { return r.Private })
}

// splitPrivate separates the torrents safe to show while locked from those
// that aren't.
func splitPrivate(statuses []torrent.Status, assignments map[string]store.Assignment, rs []rules.Rule) (public, private []torrent.Status) {
	for _, st := range statuses {
		if privateTorrent(st.Torrent, assignments[st.Hash], rs) {
			private = append(private, st)
			continue
		}
		public = append(public, st)
	}
	return public, private
}

// visibleStatuses drops private torrents unless r's page is unlocked.
func (s *Server) visibleStatuses(r *http.Request, statuses []torrent.Status, assignments map[string]store.Assignment) ([]torrent.Status, error) {
	if !s.concealing(r) {
		return statuses, nil
	}
	rs, err := s.store.Rules(r.Context())
	if err != nil {
		return nil, err
	}
	public, _ := splitPrivate(statuses, assignments, rs)
	return public, nil
}

// visibleRules drops private rules unless r's page is unlocked.
func (s *Server) visibleRules(r *http.Request, rs []rules.Rule) []rules.Rule {
	if !s.concealing(r) {
		return rs
	}
	return slices.DeleteFunc(slices.Clone(rs), func(o rules.Rule) bool { return o.Private })
}

// concealsPlacement reports whether showing p to r would reveal a private
// rule or its folder.
func (s *Server) concealsPlacement(r *http.Request, p rules.Placement) bool {
	if !s.concealing(r) {
		return false
	}
	private := func(o rules.Rule) bool { return o.Private }
	return (p.Rule != nil && p.Rule.Private) || slices.ContainsFunc(p.Ties, private)
}

// showUnlock renders the passcode prompt, or the form choosing one when none
// is set yet.
func (s *Server) showUnlock(w http.ResponseWriter, r *http.Request) {
	_, set, err := s.store.Passcode(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	title := "Unlock"
	if !set {
		title = "Choose a passcode"
	}
	s.page(w, http.StatusOK, "unlock", struct {
		chrome
		Choosing bool
	}{chrome{title}, !set})
}

// unlock checks the passcode, or sets it when none exists, and returns a key
// for the page to send with its requests. Failures are plain text for the
// prompt to show.
func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	if s.unlocks.waiting() {
		http.Error(w, "Too many attempts. Wait a moment and try again.", http.StatusTooManyRequests)
		return
	}
	passcode := r.FormValue("passcode")
	hash, set, err := s.store.Passcode(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if set {
		err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(passcode))
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			s.unlocks.failed()
			http.Error(w, "Incorrect passcode.", http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	} else {
		if msg := passcodeProblem(passcode, r.FormValue("confirm")); msg != "" {
			http.Error(w, msg, http.StatusUnprocessableEntity)
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(passcode), bcrypt.DefaultCost)
		if err != nil {
			s.fail(w, err)
			return
		}
		if err := s.store.SetPasscode(r.Context(), string(h)); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"key": s.unlocks.issue()})
}

func passcodeProblem(passcode, confirm string) string {
	switch {
	case len(passcode) < minPasscode:
		return "Use at least 4 characters."
	case len(passcode) > maxPasscode:
		return "Use at most 72 bytes."
	case passcode != confirm:
		return "The passcodes don't match."
	}
	return ""
}

// lock ends the requesting page's unlock.
func (s *Server) lock(w http.ResponseWriter, r *http.Request) {
	s.unlocks.revoke(requestKey(r))
	w.WriteHeader(http.StatusNoContent)
}
