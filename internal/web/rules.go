package web

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cwood/torrent-org/internal/intake"
	"github.com/cwood/torrent-org/internal/rules"
)

// defaultColor is a new rule's swatch until the user picks one.
const defaultColor = "#4f8cff"

type ruleForm struct {
	chrome
	Rule       rules.Rule
	Conditions []conditionRow
	Errors     []string
	// Return is where to go after saving or cancelling, when the editor was
	// opened from somewhere other than the rules list.
	Return string
}

// conditionRow is one editable condition with the choices for its selects.
type conditionRow struct {
	Condition rules.Condition
	Fields    []rules.Field
	Ops       []rules.Op
}

func newConditionRow(c rules.Condition) conditionRow {
	return conditionRow{Condition: c, Fields: rules.Fields, Ops: rules.Ops}
}

func (s *Server) ruleForm(req *http.Request, r rules.Rule, err error) ruleForm {
	title := "Edit rule"
	if r.ID == 0 {
		title = "New rule"
	}
	f := ruleForm{chrome: chrome{title, "rules"}, Rule: r, Return: returnTo(req)}
	for _, c := range r.Conditions {
		f.Conditions = append(f.Conditions, newConditionRow(c))
	}
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

func (s *Server) newRule(w http.ResponseWriter, r *http.Request) {
	s.page(w, http.StatusOK, "rule", s.ruleForm(r, rules.Rule{
		Color:      defaultColor,
		Mode:       rules.ModeAll,
		Path:       "{root}/{tracker}/",
		Conditions: []rules.Condition{{Field: rules.FieldExtension, Op: rules.OpIs}},
	}, nil))
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

// condition renders an empty condition row for the "Add condition" button.
func (s *Server) condition(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "rule", "condition", newConditionRow(rules.Condition{Field: rules.FieldExtension, Op: rules.OpIs}))
}

// parseRule reads a rule from the editor form. Conditions arrive as parallel
// field/op/value lists, one entry per row.
func parseRule(r *http.Request) rules.Rule {
	f := r.PostForm
	rule := rules.Rule{
		Name:  strings.TrimSpace(f.Get("name")),
		Color: f.Get("color"),
		Mode:  rules.Mode(f.Get("mode")),
		Path:  strings.TrimSpace(f.Get("path")),
	}
	fields, ops, values := f["field"], f["op"], f["value"]
	for i := range min(len(fields), len(ops), len(values)) {
		rule.Conditions = append(rule.Conditions, rules.Condition{
			Field: rules.Field(fields[i]),
			Op:    rules.Op(ops[i]),
			Value: strings.TrimSpace(values[i]),
		})
	}
	return rule
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
	if r.PathValue("id") == "" {
		rule.Priority = int64(len(rs) + 1)
	} else {
		existing, ok, err := s.findRule(r)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		rule.ID, rule.Priority = existing.ID, existing.Priority
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
	changes, err := s.changes(r, rs, trackers, id)
	if err != nil {
		return err
	}
	var moves []intake.Move
	for _, c := range changes {
		if !c.Assignment.Manual && slices.Contains(selected, c.Hash) {
			moves = append(moves, intake.Move{Hash: c.Hash, Placement: c.Placement})
		}
	}
	if len(moves) == 0 {
		return nil
	}
	return s.intake.Relocate(r.Context(), moves)
}

// changes plans every torrent against rs and keeps the changes caused by the
// rule with id, sorted by name.
func (s *Server) changes(r *http.Request, rs []rules.Rule, trackers map[string]string, id int64) ([]intake.Change, error) {
	statuses, err := s.client.Torrents(r.Context())
	if err != nil {
		return nil, err
	}
	assignments, err := s.store.Assignments(r.Context())
	if err != nil {
		return nil, err
	}
	var out []intake.Change
	for _, c := range intake.Plan(statuses, assignments, rs, trackers, s.root) {
		if affects(c, id) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b intake.Change) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
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

// moveRule swaps a rule with its neighbour; the form's dir is "up" or "down".
func (s *Server) moveRule(w http.ResponseWriter, r *http.Request) {
	rule, ok, err := s.findRule(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	rs, err := s.store.Rules(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ids := make([]int64, len(rs))
	for i, o := range rs {
		ids[i] = o.ID
	}
	i := slices.Index(ids, rule.ID)
	j := i + 1
	if r.FormValue("dir") == "up" {
		j = i - 1
	}
	if j >= 0 && j < len(ids) {
		ids[i], ids[j] = ids[j], ids[i]
		if err := s.store.ReorderRules(r.Context(), ids); err != nil {
			s.fail(w, err)
			return
		}
	}
	redirect(w, r, "/rules")
}

// previewRule lists the torrents saving the rule being edited would move or
// regroup, taking the other rules' priority into account.
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
	// Evaluate as if saved: replace the stored rule, or append a new one,
	// which saveRule gives the lowest priority.
	if i := slices.IndexFunc(rs, func(o rules.Rule) bool { return o.ID == rule.ID && rule.ID != 0 }); i >= 0 {
		rule.Priority = rs[i].Priority
		rs[i] = rule
	} else {
		rs = append(rs, rule)
	}

	data := struct {
		Changes []intake.Change
		Manual  bool
		Error   string
	}{Manual: len(rule.Conditions) == 0}
	data.Changes, err = s.changes(r, rs, trackers, rule.ID)
	if err != nil {
		data.Error = err.Error()
	}
	s.render(w, http.StatusOK, "rule", "preview", data)
}
