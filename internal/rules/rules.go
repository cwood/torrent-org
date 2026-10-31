// Package rules decides where a torrent belongs. It mirrors the auto-assign
// groups in the macOS Transmission client: rules with conditions combined by
// "all", "any" or "none" and nestable in groups. Unlike the macOS client,
// rules are independent of each other rather than ordered: when several
// match, the most specific wins, and ties are left for the user to settle.
package rules

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/cwood/torrent-org/internal/torrent"
)

// Field is the torrent attribute a condition inspects.
type Field string

const (
	FieldName      Field = "name"
	FieldTracker   Field = "tracker"
	FieldExtension Field = "extension"
)

// Fields lists every Field in display order.
var Fields = []Field{FieldName, FieldTracker, FieldExtension}

// Op is the string comparison a condition applies. Comparisons are
// case-insensitive, as in the macOS client.
type Op string

const (
	OpIs         Op = "is"
	OpContains   Op = "contains"
	OpBeginsWith Op = "begins_with"
	OpEndsWith   Op = "ends_with"
)

// Ops lists every Op in display order.
var Ops = []Op{OpIs, OpContains, OpBeginsWith, OpEndsWith}

// Mode is how a group combines its conditions and nested groups.
type Mode string

const (
	ModeAll  Mode = "all"
	ModeAny  Mode = "any"
	ModeNone Mode = "none"
)

// Modes lists every Mode in display order.
var Modes = []Mode{ModeAll, ModeAny, ModeNone}

// Untracked is the tracker directory name used for torrents with no
// parseable announce URL.
const Untracked = "untracked"

// Condition is a single comparison against a torrent attribute.
//
// FieldTracker is satisfied if any of the torrent's tracker hosts satisfies
// the comparison. FieldExtension compares against the torrent's dominant
// extension (see [torrent.Torrent.DominantExtension]); a leading dot in Value
// is ignored.
type Condition struct {
	Field Field  `json:"field"`
	Op    Op     `json:"op"`
	Value string `json:"value"`
}

// Group combines conditions and nested groups by its Mode, like a compound
// row in the macOS client's rule editor.
type Group struct {
	Mode       Mode        `json:"mode"`
	Conditions []Condition `json:"conditions,omitempty"`
	Groups     []Group     `json:"groups,omitempty"`
}

// HasConditions reports whether g or any group nested in it has a
// condition.
func (g Group) HasConditions() bool {
	return len(g.Conditions) > 0 || slices.ContainsFunc(g.Groups, Group.HasConditions)
}

// score is how specifically t matches g: the number of conditions that held
// on the way to the match, or -1 when g doesn't match. Conditions and groups
// a "none" group excludes count once each, since they narrowed the match
// too.
func (g Group) score(t torrent.Torrent) int {
	results := make([]int, 0, len(g.Conditions)+len(g.Groups))
	for _, c := range g.Conditions {
		if c.matches(t) {
			results = append(results, 1)
		} else {
			results = append(results, -1)
		}
	}
	for _, sub := range g.Groups {
		results = append(results, sub.score(t))
	}

	n := 0
	switch g.Mode {
	case ModeAll:
		for _, s := range results {
			if s < 0 {
				return -1
			}
			n += s
		}
		return n
	case ModeAny:
		matched := false
		for _, s := range results {
			if s >= 0 {
				matched = true
				n += s
			}
		}
		if !matched {
			return -1
		}
		return n
	case ModeNone:
		if slices.ContainsFunc(results, func(s int) bool { return s >= 0 }) {
			return -1
		}
		return len(results)
	}
	return -1
}

// String renders g as one line, such as
// "tracker is example.org and (extension is mkv or extension is mp4)".
func (g Group) String() string {
	var parts []string
	for _, c := range g.Conditions {
		parts = append(parts, fmt.Sprintf("%s %s %s", c.Field, strings.ReplaceAll(string(c.Op), "_", " "), c.Value))
	}
	for _, sub := range g.Groups {
		s := sub.String()
		if sub.Mode != ModeNone && len(sub.Conditions)+len(sub.Groups) > 1 {
			s = "(" + s + ")"
		}
		parts = append(parts, s)
	}
	switch g.Mode {
	case ModeAny:
		return strings.Join(parts, " or ")
	case ModeNone:
		return "none of (" + strings.Join(parts, ", ") + ")"
	}
	return strings.Join(parts, " and ")
}

// Rule is a named group with auto-assign conditions and an optional
// download location.
//
// Path may reference {root} and {tracker}. An empty Path places matching
// torrents in the default location, like a macOS group without a custom
// download location.
type Rule struct {
	ID    int64
	Name  string
	Color string
	Path  string
	Match Group
}

// Placement is where a torrent should live and which group it belongs to.
// Rule is nil and Group empty when no single rule won; Ties then lists the
// rules that matched equally well, if any, and the placement is the default
// location.
type Placement struct {
	Rule    *Rule
	Ties    []Rule
	Tracker string
	Dir     string
	Group   string
}

// Matches reports whether t satisfies r's conditions. A rule without
// conditions never matches, so it can only be assigned manually.
func (r Rule) Matches(t torrent.Torrent) bool {
	return r.Specificity(t) >= 0
}

// Specificity is how many of r's conditions held for t on the way to a
// match, or -1 when r doesn't match t.
func (r Rule) Specificity(t torrent.Torrent) int {
	if !r.Match.HasConditions() {
		return -1
	}
	return r.Match.score(t)
}

func (c Condition) matches(t torrent.Torrent) bool {
	switch c.Field {
	case FieldName:
		return c.compare(t.Name)
	case FieldTracker:
		return slices.ContainsFunc(t.TrackerHosts(), c.compare)
	case FieldExtension:
		return c.compare(t.DominantExtension())
	}
	return false
}

func (c Condition) compare(s string) bool {
	s = strings.ToLower(s)
	v := strings.ToLower(c.Value)
	if c.Field == FieldExtension {
		v = strings.TrimPrefix(v, ".")
	}
	switch c.Op {
	case OpIs:
		return s == v
	case OpContains:
		return strings.Contains(s, v)
	case OpBeginsWith:
		return strings.HasPrefix(s, v)
	case OpEndsWith:
		return strings.HasSuffix(s, v)
	}
	return false
}

// Match returns the rules in rs that t matches most specifically: none when
// nothing matches, one when a rule wins, and several when they tie.
func Match(t torrent.Torrent, rs []Rule) []Rule {
	var best []Rule
	top := -1
	for _, r := range rs {
		switch s := r.Specificity(t); {
		case s > top:
			best, top = []Rule{r}, s
		case s == top && s >= 0:
			best = append(best, r)
		}
	}
	return best
}

// Place resolves where t should be stored under root. trackers maps announce
// hosts to directory names; the first of t's hosts with a mapping wins,
// otherwise the first host is used as-is.
func Place(t torrent.Torrent, rs []Rule, trackers map[string]string, root string) Placement {
	best := Match(t, rs)
	if len(best) == 1 {
		return PlaceIn(t, &best[0], trackers, root)
	}
	p := PlaceIn(t, nil, trackers, root)
	if len(best) > 1 {
		p.Ties = best
	}
	return p
}

// PlaceIn resolves where t should be stored if assigned to r regardless of
// r's conditions, or to the default location when r is nil.
func PlaceIn(t torrent.Torrent, r *Rule, trackers map[string]string, root string) Placement {
	p := Placement{Rule: r, Tracker: trackerName(t, trackers)}
	if r != nil {
		p.Group = r.Name
	}
	p.Dir = p.dir(root)
	return p
}

func (p Placement) dir(root string) string {
	tmpl := "{root}/{tracker}"
	if p.Rule != nil && p.Rule.Path != "" {
		tmpl = p.Rule.Path
	}
	return path.Clean(strings.NewReplacer("{root}", root, "{tracker}", p.Tracker).Replace(tmpl))
}

func trackerName(t torrent.Torrent, trackers map[string]string) string {
	hosts := t.TrackerHosts()
	for _, h := range hosts {
		if name, ok := trackers[h]; ok {
			return name
		}
	}
	if len(hosts) > 0 {
		return hosts[0]
	}
	return Untracked
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate reports problems with a rule entered by a user.
func (r Rule) Validate() error {
	var errs []error
	switch {
	case strings.TrimSpace(r.Name) == "":
		errs = append(errs, errors.New("name is required"))
	case strings.Contains(r.Name, ","):
		// Transmission rejects labels containing commas, and the name is
		// used as the label.
		errs = append(errs, errors.New("name cannot contain commas"))
	}
	if !colorPattern.MatchString(r.Color) {
		errs = append(errs, fmt.Errorf("color %q must look like #4f8cff", r.Color))
	}
	if r.Path != "" && !strings.HasPrefix(r.Path, "{root}") && !path.IsAbs(r.Path) {
		errs = append(errs, fmt.Errorf("path %q must start with {root} or /", r.Path))
	}
	var n int
	errs = append(errs, r.Match.validate(true, &n)...)
	return errors.Join(errs...)
}

// validate checks g and its nested groups. Conditions are numbered in
// display order through n. Only the top-level group may be empty, which
// makes a rule that is assigned by hand.
func (g Group) validate(top bool, n *int) []error {
	var errs []error
	if !slices.Contains(Modes, g.Mode) {
		errs = append(errs, fmt.Errorf("mode %q must be all, any or none", g.Mode))
	}
	if !top && len(g.Conditions)+len(g.Groups) == 0 {
		errs = append(errs, errors.New("nested groups need at least one condition"))
	}
	for _, c := range g.Conditions {
		*n++
		if !slices.Contains(Fields, c.Field) {
			errs = append(errs, fmt.Errorf("condition %d: unknown field %q", *n, c.Field))
		}
		if !slices.Contains(Ops, c.Op) {
			errs = append(errs, fmt.Errorf("condition %d: unknown comparison %q", *n, c.Op))
		}
		if c.Value == "" {
			errs = append(errs, fmt.Errorf("condition %d: value is required", *n))
		}
	}
	for _, sub := range g.Groups {
		errs = append(errs, sub.validate(false, n)...)
	}
	return errs
}
