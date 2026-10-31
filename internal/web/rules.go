package web

import (
	"cmp"
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store"
	"github.com/cwood/torrent-org/internal/torrent"
)

// defaultColor is a new rule's colour until the user picks one: the colour
// the macOS Transmission client gives a new group.
const defaultColor = "#00a6ff"

// swatch is a preset colour offered in the rule editor.
type swatch struct {
	Name  string
	Color string
}

// swatches are the macOS client's default groups, which use Apple's system
// colours.
var swatches = []swatch{
	{"Red", "#ff3b30"},
	{"Orange", "#ff9500"},
	{"Yellow", "#ffcc00"},
	{"Green", "#28cd41"},
	{"Blue", "#007aff"},
	{"Purple", "#af52de"},
	{"Gray", "#8e8e93"},
}

type ruleForm struct {
	chrome
	Rule   rules.Rule
	Match  groupView
	Errors []string
	// Return is where to go after saving or cancelling, when the editor was
	// opened from somewhere other than the rules list.
	Return   string
	Swatches []swatch
}

// blankCondition is the row the editor adds for "Add condition".
var blankCondition = rules.Condition{Field: rules.FieldExtension, Op: rules.OpIs}

// conditionRow is one editable condition with the choices for its selects.
// Group is the ID of the group it belongs to.
type conditionRow struct {
	Condition rules.Condition
	Group     string
	Fields    []rules.Field
	Ops       []rules.Op
}

func newConditionRow(c rules.Condition, group string) conditionRow {
	return conditionRow{Condition: c, Group: group, Fields: rules.Fields, Ops: rules.Ops}
}

// groupView is one editable group. ID ties the group to its rows and nested
// groups in the posted form; IDs only need to be unique within a page.
// Parent is empty for the top-level group.
type groupView struct {
	ID         string
	Parent     string
	Mode       rules.Mode
	Modes      []rules.Mode
	Conditions []conditionRow
	Groups     []groupView
}

func newGroupView(g rules.Group, parent string) groupView {
	v := groupView{ID: rand.Text(), Parent: parent, Mode: g.Mode, Modes: rules.Modes}
	for _, c := range g.Conditions {
		v.Conditions = append(v.Conditions, newConditionRow(c, v.ID))
	}
	for _, sub := range g.Groups {
		v.Groups = append(v.Groups, newGroupView(sub, v.ID))
	}
	return v
}

func (s *Server) ruleForm(req *http.Request, r rules.Rule, err error) ruleForm {
	title := "Edit rule"
	if r.ID == 0 {
		title = "New rule"
	}
	f := ruleForm{chrome: chrome{title, "rules"}, Rule: r, Match: newGroupView(r.Match, ""), Return: returnTo(req), Swatches: swatches}
	if err != nil {
		f.Errors = strings.Split(err.Error(), "\n")
	}
	return f
}

func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	s.rulesPage(w, r, http.StatusOK, "")
}

func (s *Server) rulesPage(w http.ResponseWriter, r *http.Request, code int, errMsg string) {
	rs, err := s.store.Rules(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.page(w, code, "rules", struct {
		chrome
		Rules []rules.Rule
		Error string
	}{chrome{"Rules", "rules"}, rs, errMsg})
}

// newRule opens the editor for a new rule, or for a copy of the rule named
// by the from query parameter.
func (s *Server) newRule(w http.ResponseWriter, r *http.Request) {
	rule := rules.Rule{
		Color: defaultColor,
		Path:  "{root}/{tracker}/",
		Match: rules.Group{Mode: rules.ModeAll, Conditions: []rules.Condition{blankCondition}},
	}
	if from := r.URL.Query().Get("from"); from != "" {
		rs, err := s.store.Rules(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		i := slices.IndexFunc(rs, func(o rules.Rule) bool { return strconv.FormatInt(o.ID, 10) == from })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		rule = rs[i]
		rule.ID, rule.Name = 0, rule.Name+" copy"
	}
	s.page(w, http.StatusOK, "rule", s.ruleForm(r, rule, nil))
}

func (s *Server) editRule(w http.ResponseWriter, r *http.Request) {
	rule, ok, err := s.findRule(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.page(w, http.StatusOK, "rule", s.ruleForm(r, rule, nil))
}

// findRule loads the rule named by the {id} path value.
func (s *Server) findRule(r *http.Request) (rules.Rule, bool, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return rules.Rule{}, false, nil
	}
	rs, err := s.store.Rules(r.Context())
	if err != nil {
		return rules.Rule{}, false, err
	}
	i := slices.IndexFunc(rs, func(r rules.Rule) bool { return r.ID == id })
	if i < 0 {
		return rules.Rule{}, false, nil
	}
	return rs[i], true, nil
}

// condition renders an empty condition row for a group's "Add condition"
// button.
func (s *Server) condition(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "rule", "condition", newConditionRow(blankCondition, r.URL.Query().Get("group")))
}

// group renders a nested group holding one empty condition for a group's
// "Add group" button.
func (s *Server) group(w http.ResponseWriter, r *http.Request) {
	g := rules.Group{Mode: rules.ModeAny, Conditions: []rules.Condition{blankCondition}}
	s.render(w, http.StatusOK, "rule", "group", newGroupView(g, r.URL.Query().Get("parent")))
}

// parseRule reads a rule from the editor form.
func parseRule(r *http.Request) rules.Rule {
	f := r.PostForm
	return rules.Rule{
		Name:  strings.TrimSpace(f.Get("name")),
		Color: f.Get("color"),
		Path:  strings.TrimSpace(f.Get("path")),
		Match: parseGroups(f),
	}
}

// parseGroups rebuilds the condition tree from the editor form. Each group
// posts parallel group/group_parent/group_mode entries, and each condition
// row posts its group's ID in cond_group alongside field, op and value, so
// rows and groups can be removed in the browser in any order.
//
// The first group without a parent is the top level. Every other group has
// exactly one parent and duplicate IDs are ignored, so walking down from the
// top level visits a tree; groups that don't hang off it, including crafted
// cycles, are dropped.
func parseGroups(f url.Values) rules.Group {
	ids, parents, modes := f["group"], f["group_parent"], f["group_mode"]
	groups := make(map[string]*rules.Group)
	children := make(map[string][]string)
	var top string
	for i := range min(len(ids), len(parents), len(modes)) {
		id := ids[i]
		if _, dup := groups[id]; dup || id == "" {
			continue
		}
		groups[id] = &rules.Group{Mode: rules.Mode(modes[i])}
		if parents[i] == "" && top == "" {
			top = id
			continue
		}
		children[parents[i]] = append(children[parents[i]], id)
	}
	if top == "" {
		return rules.Group{Mode: rules.ModeAll}
	}

	owners, fields, ops, values := f["cond_group"], f["field"], f["op"], f["value"]
	for i := range min(len(owners), len(fields), len(ops), len(values)) {
		if g, ok := groups[owners[i]]; ok {
			g.Conditions = append(g.Conditions, rules.Condition{
				Field: rules.Field(fields[i]),
				Op:    rules.Op(ops[i]),
				Value: strings.TrimSpace(values[i]),
			})
		}
	}

	var build func(id string) rules.Group
	build = func(id string) rules.Group {
		g := *groups[id]
		for _, child := range children[id] {
			g.Groups = append(g.Groups, build(child))
		}
		return g
	}
	return build(top)
}

func (s *Server) saveRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rule := parseRule(r)

	rs, err := s.store.Rules(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.PathValue("id") != "" {
		existing, ok, err := s.findRule(r)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		rule.ID = existing.ID
	}

	err = rule.Validate()
	if err == nil && slices.ContainsFunc(rs, func(o rules.Rule) bool { return o.Name == rule.Name && o.ID != rule.ID }) {
		err = errors.New("another rule already has that name")
	}
	if err != nil {
		s.page(w, http.StatusUnprocessableEntity, "rule", s.ruleForm(r, rule, err))
		return
	}
	id, err := s.store.SaveRule(r.Context(), rule)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.applyRule(r, id, r.PostForm["move"]); err != nil {
		s.rulesPage(w, r, http.StatusBadGateway, "Rule saved, but some torrents could not be moved: "+err.Error())
		return
	}
	redirect(w, r, cmp.Or(returnTo(r), "/rules"))
}

// returnTo is the request's "return" value if it points back at an add
// preview, the only page that opens the editor elsewhere. Anything else is
// dropped so the value can't redirect off-site.
func returnTo(r *http.Request) string {
	ret := r.FormValue("return")
	if strings.HasPrefix(ret, "/add/") && !strings.ContainsAny(ret[len("/add/"):], "/\\?#") {
		return ret
	}
	return ""
}

// applyRule relocates the selected torrents whose placement the saved rule
// with id changes. The plan is recomputed here rather than trusting paths
// from the browser; selected only filters it.
func (s *Server) applyRule(r *http.Request, id int64, selected []string) error {
	if len(selected) == 0 {
		return nil
	}
	rs, trackers, err := s.ruleset(r.Context())
	if err != nil {
		return err
	}
	statuses, assignments, err := s.holdings(r)
	if err != nil {
		return err
	}
	var moves []intake.Move
	for _, c := range changes(statuses, assignments, rs, trackers, s.root, id) {
		if !c.Assignment.Manual && slices.Contains(selected, c.Hash) {
			moves = append(moves, intake.Move{Hash: c.Hash, Placement: c.Placement})
		}
	}
	if len(moves) == 0 {
		return nil
	}
	return s.intake.Relocate(r.Context(), moves)
}

// holdings fetches what placement previews need: every torrent the client
// holds and its assignment.
func (s *Server) holdings(r *http.Request) ([]torrent.Status, map[string]store.Assignment, error) {
	statuses, err := s.client.Torrents(r.Context())
	if err != nil {
		return nil, nil, err
	}
	assignments, err := s.store.Assignments(r.Context())
	if err != nil {
		return nil, nil, err
	}
	return statuses, assignments, nil
}

// changes plans every torrent against rs and keeps the changes caused by the
// rule with id, sorted by name.
func changes(statuses []torrent.Status, assignments map[string]store.Assignment, rs []rules.Rule, trackers map[string]string, root string, id int64) []intake.Change {
	var out []intake.Change
	for _, c := range intake.Plan(statuses, assignments, rs, trackers, root) {
		if affects(c, id) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b intake.Change) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// matches sorts the torrents rule's conditions match within rs. held are
// those it would hold: pinned to it, or unpinned with it as the single most
// specific match. tied are unpinned ones where it ties with other rules, so
// the user has to choose. taken counts those a more specific rule or a pin
// elsewhere claims.
func matches(statuses []torrent.Status, assignments map[string]store.Assignment, rs []rules.Rule, rule rules.Rule) (held, tied []row, taken int) {
	for _, st := range statuses {
		a := assignments[st.Hash]
		if a.Manual && rule.ID != 0 && a.RuleID == rule.ID {
			held = append(held, row{Status: st, Rule: &rule, Manual: true})
			continue
		}
		if !rule.Matches(st.Torrent) {
			continue
		}
		best := rules.Match(st.Torrent, rs)
		switch {
		case a.Manual:
			taken++
		case len(best) == 1 && best[0].ID == rule.ID:
			held = append(held, row{Status: st, Rule: &rule})
		case slices.ContainsFunc(best, func(o rules.Rule) bool { return o.ID == rule.ID }):
			tied = append(tied, row{Status: st, Ties: best})
		default:
			taken++
		}
	}
	byName := func(a, b row) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(held, byName)
	slices.SortFunc(tied, byName)
	return held, tied, taken
}

// previewLimit is how many rows each list in the "On save" panel shows
// before folding the rest behind an expander.
const previewLimit = 5

// folded is a list shown as its first rows with the rest behind an
// expander.
type folded[T any] struct {
	Head, Tail []T
}

func fold[T any](s []T) folded[T] {
	n := min(len(s), previewLimit)
	return folded[T]{Head: s[:n], Tail: s[n:]}
}

// Len is the number of rows in the whole list.
func (f folded[T]) Len() int {
	return len(f.Head) + len(f.Tail)
}

// affects reports whether the rule with id causes c: it now claims the
// torrent, or it placed the torrent before. ID 0 is an unsaved rule, and
// assignments also use 0 for the default location, so only the first test
// applies to it.
func affects(c intake.Change, id int64) bool {
	if c.Placement.Rule != nil && c.Placement.Rule.ID == id {
		return true
	}
	return id != 0 && c.Assignment.RuleID == id
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	rule, ok, err := s.findRule(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if ok {
		if err := s.store.DeleteRule(r.Context(), rule.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	redirect(w, r, "/rules")
}

// previewRule shows the torrents the rule being edited would hold if saved,
// where it ties with other rules, and the ones saving it would move or
// regroup.
func (s *Server) previewRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rule := parseRule(r)
	rule.ID, _ = strconv.ParseInt(r.PostForm.Get("id"), 10, 64)

	rs, trackers, err := s.ruleset(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	// Evaluate as if saved: replace the stored rule, or add a new one.
	if i := slices.IndexFunc(rs, func(o rules.Rule) bool { return o.ID == rule.ID && rule.ID != 0 }); i >= 0 {
		rs[i] = rule
	} else {
		rs = append(rs, rule)
	}

	data := struct {
		Color   string
		Matches folded[row]
		Ties    folded[row]
		Taken   int
		Changes folded[intake.Change]
		// Moving counts the changes that will move; manual ones are listed
		// but never moved.
		Moving int
		Manual bool
		Error  string
	}{Color: rule.Color, Manual: !rule.Match.HasConditions()}
	statuses, assignments, err := s.holdings(r)
	if err != nil {
		data.Error = err.Error()
		s.render(w, http.StatusOK, "rule", "preview", data)
		return
	}
	held, tied, taken := matches(statuses, assignments, rs, rule)
	data.Matches, data.Ties, data.Taken = fold(held), fold(tied), taken
	cs := changes(statuses, assignments, rs, trackers, s.root, rule.ID)
	data.Changes = fold(cs)
	for _, c := range cs {
		if !c.Assignment.Manual {
			data.Moving++
		}
	}
	s.render(w, http.StatusOK, "rule", "preview", data)
}
