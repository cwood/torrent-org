package web

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

// Rule picker choices besides a rule ID.
const (
	choiceAuto = "auto"
	choiceNone = "none"
)

// picker is the data for the shared rule picker partial. Changing the
// selection posts the enclosing form to Action and swaps the element with id
// Target from the response.
type picker struct {
	Rules  []rules.Rule
	Choice string
	Action string
	Target string
}

// ruleset loads what placement decisions need: rules in name order and
// tracker aliases.
func (s *Server) ruleset(ctx context.Context) ([]rules.Rule, map[string]string, error) {
	rs, err := s.store.Rules(ctx)
	if err != nil {
		return nil, nil, err
	}
	trackers, err := s.store.Trackers(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rs, trackers, nil
}

// placeChoice resolves where t goes for a picker choice: the rules' pick,
// the default location, or a specific rule ID.
func (s *Server) placeChoice(t torrent.Torrent, choice string, rs []rules.Rule, trackers map[string]string) (rules.Placement, error) {
	switch choice {
	case choiceAuto:
		return rules.Place(t, rs, trackers, s.root), nil
	case choiceNone:
		return rules.PlaceIn(t, nil, trackers, s.root), nil
	}
	id, _ := strconv.ParseInt(choice, 10, 64)
	i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.ID == id })
	if i < 0 {
		return rules.Placement{}, errors.New("that rule no longer exists")
	}
	return rules.PlaceIn(t, &rs[i], trackers, s.root), nil
}

// choiceOf is the picker choice that reproduces an existing assignment.
func choiceOf(a store.Assignment) string {
	switch {
	case !a.Manual:
		return choiceAuto
	case a.RuleID == 0:
		return choiceNone
	}
	return strconv.FormatInt(a.RuleID, 10)
}
