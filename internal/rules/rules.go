// Package rules decides where a torrent belongs. It mirrors the auto-assign
// groups in the macOS Transmission client: an ordered list of rules, each with
// conditions combined by "all" or "any", where the first matching rule wins.
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

// Mode is how a rule combines its conditions.
type Mode string

const (
	ModeAll Mode = "all"
	ModeAny Mode = "any"
)

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
	Field Field
	Op    Op
	Value string
}

// Rule is a named group with auto-assign conditions and an optional
// download location.
//
// Path may reference {root} and {tracker}. An empty Path places matching
// torrents in the default location, like a macOS group without a custom
// download location.
type Rule struct {
	ID         int64
	Name       string
	Color      string
	Priority   int64
	Mode       Mode
	Path       string
	Conditions []Condition
}

// Placement is where a torrent should live and which group it belongs to.
// Rule is nil and Group empty when no rule matched.
type Placement struct {
	Rule    *Rule
	Tracker string
	Dir     string
	Group   string
}

// Matches reports whether t satisfies r's conditions. A rule without
// conditions never matches, so it can only be assigned manually.
func (r Rule) Matches(t torrent.Torrent) bool {
	if len(r.Conditions) == 0 {
		return false
	}
	for _, c := range r.Conditions {
		ok := c.matches(t)
		if ok && r.Mode == ModeAny {
			return true
		}
		if !ok && r.Mode == ModeAll {
			return false
		}
	}
	return r.Mode == ModeAll
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

// Match returns the first rule in rs that t satisfies. rs must already be in
// priority order.
func Match(t torrent.Torrent, rs []Rule) (Rule, bool) {
	i := slices.IndexFunc(rs, func(r Rule) bool { return r.Matches(t) })
	if i < 0 {
		return Rule{}, false
	}
	return rs[i], true
}

// Place resolves where t should be stored under root. trackers maps announce
// hosts to directory names; the first of t's hosts with a mapping wins,
// otherwise the first host is used as-is.
func Place(t torrent.Torrent, rs []Rule, trackers map[string]string, root string) Placement {
	r, ok := Match(t, rs)
	if !ok {
		return PlaceIn(t, nil, trackers, root)
	}
	return PlaceIn(t, &r, trackers, root)
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
	if r.Mode != ModeAll && r.Mode != ModeAny {
		errs = append(errs, fmt.Errorf("mode %q must be all or any", r.Mode))
	}
	if r.Path != "" && !strings.HasPrefix(r.Path, "{root}") && !path.IsAbs(r.Path) {
		errs = append(errs, fmt.Errorf("path %q must start with {root} or /", r.Path))
	}
	for i, c := range r.Conditions {
		if !slices.Contains(Fields, c.Field) {
			errs = append(errs, fmt.Errorf("condition %d: unknown field %q", i+1, c.Field))
		}
		if !slices.Contains(Ops, c.Op) {
			errs = append(errs, fmt.Errorf("condition %d: unknown comparison %q", i+1, c.Op))
		}
		if c.Value == "" {
			errs = append(errs, fmt.Errorf("condition %d: value is required", i+1))
		}
	}
	return errors.Join(errs...)
}
