package litebuild

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Sources of the values in a Step.
const (
	SourceAnswer = "answer"       // the user answered the question
	SourceOnly   = "only allowed" // the only allowed value: no question
	SourceRule   = "rule"         // a rule set it
)

// Question is one question to the user.
type Question struct {
	Attr *Attribute
	// Options are the allowed answers, in enum order (false then true for a
	// boolean). It is nil for an attribute without enum: the answer is a
	// text.
	Options []any
	// Default is the preselected answer: the previous answer
	// (Configurator.Previous) when it is allowed, else display.default when
	// it is allowed, else nil.
	Default any
}

// Prompter asks the questions.
type Prompter interface {
	// Select asks to choose one of q.Options and returns its index.
	Select(q Question) (int, error)
	// Confirm asks a yes/no question for a boolean attribute.
	Confirm(q Question) (bool, error)
	// Text asks a value for an attribute without enum. validate returns
	// nil when the text is a valid answer.
	Text(q Question, validate func(string) error) (string, error)
	// Info shows a value that needs no question.
	Info(attr *Attribute, value any, reason string)
}

// Step is one attribute of the run, in question order.
type Step struct {
	Attr   *Attribute
	Value  any
	Source string // SourceAnswer, SourceOnly or SourceRule
	Rule   *Rule  // for SourceRule: the rule that set the value
}

// Result is the result of Run.
type Result struct {
	// Inputs are the answers and the only allowed values: the build
	// profile. solve.py gives the same solution with these values.
	Inputs   map[string]any
	Solution *Solution
	Steps    []Step
}

// ErrNoConfiguration means that the model has no allowed configuration.
var ErrNoConfiguration = errors.New("the master configuration has no allowed configuration")

// Configurator asks the questions of a model.
//
// The questions follow the question order, a topological order of the
// attributes. The rules are evaluated during the walk, as in Evaluate: a
// rule is applied at the first of its targets in the question order. All
// the attributes in its condition come before, so their values are final
// (defined, or undefined for good). Thus no rule ever sees an attribute
// that is undefined only because its question is not asked yet.
type Configurator struct {
	solver   *Solver
	required map[string][]string
	order    []*Attribute    // question order
	relevant map[string]bool // required attributes and their dependencies
	// rulesAt[k]: the rules (not valid: false) applied at position k, file
	// order. Position len(order) is sdkconfig: the rules that only add lines.
	rulesAt [][]*Rule
	// forbiddenAt[k]: the valid: false rules whose condition reads only
	// attributes before position k: their result is final at position k.
	forbiddenAt [][]*Rule

	// Previous are the values of an earlier run, by attribute name (see
	// LoadAnswers). The previous value of a question is its default when it
	// is allowed.
	Previous map[string]any
}

// NewConfigurator prepares the questions. extra adds required attributes,
// as extra_required of solve.py; nil uses the extra_required of
// configure.py: chip, idf_version and idf_preview, each when the model
// declares it.
func NewConfigurator(m *Model, extra map[string][]string) (*Configurator, error) {
	solver, err := NewSolver(m)
	if err != nil {
		return nil, err
	}
	if extra == nil {
		extra = map[string][]string{}
		for name, neededBy := range map[string]string{
			Chip:       "idf.py set-target",
			IDFVersion: "ESP-IDF version check",
			IDFPreview: "idf.py --preview",
		} {
			if m.Attributes[name] != nil {
				extra[name] = []string{neededBy}
			}
		}
	}
	required, err := solver.Required(extra)
	if err != nil {
		return nil, err
	}
	c := &Configurator{
		solver:   solver,
		required: required,
		order:    questionOrder(m, solver.Graph),
		relevant: map[string]bool{},
	}

	var addRelevant func(name string)
	addRelevant = func(name string) {
		if c.relevant[name] {
			return
		}
		c.relevant[name] = true
		for _, dep := range solver.Graph[name] {
			addRelevant(dep)
		}
	}
	for name := range required {
		addRelevant(name)
	}

	pos := make(map[string]int, len(c.order)+1)
	for i, a := range c.order {
		pos[a.Name] = i
	}
	pos[Sdkconfig] = len(c.order)
	c.rulesAt = make([][]*Rule, len(c.order)+1)
	c.forbiddenAt = make([][]*Rule, len(c.order)+1)
	for _, rule := range m.Rules {
		if rule.Forbidden {
			k := 0
			for name := range rule.Reads() {
				k = max(k, pos[name]+1)
			}
			c.forbiddenAt[k] = append(c.forbiddenAt[k], rule)
			continue
		}
		k := len(c.order)
		for _, t := range rule.Targets() {
			k = min(k, pos[t])
		}
		c.rulesAt[k] = append(c.rulesAt[k], rule)
	}
	return c, nil
}

// Order returns the question order.
func (c *Configurator) Order() []*Attribute { return c.order }

// questionOrder sorts the attributes by display priority (no priority:
// last), then by declaration order. Each attribute pulls its dependencies
// before it, in the same sort. The result is a topological order that
// follows the priorities as much as the dependencies allow.
func questionOrder(m *Model, graph map[string][]string) []*Attribute {
	compare := func(a, b *Attribute) int {
		pa, pb := a.Display.Priority, b.Display.Priority
		switch {
		case pa != nil && pb != nil && *pa != *pb:
			return *pa - *pb
		case pa != nil && pb == nil:
			return -1
		case pa == nil && pb != nil:
			return 1
		}
		return a.Index - b.Index
	}
	sorted := slices.Clone(m.Order)
	slices.SortStableFunc(sorted, compare)

	emitted := map[string]bool{}
	var order []*Attribute
	var emit func(a *Attribute)
	emit = func(a *Attribute) {
		if emitted[a.Name] {
			return
		}
		emitted[a.Name] = true // the graph has no cycle (NewSolver checks it)
		deps := make([]*Attribute, 0, len(graph[a.Name]))
		for _, name := range graph[a.Name] {
			deps = append(deps, m.Attributes[name])
		}
		slices.SortStableFunc(deps, compare)
		for _, d := range deps {
			emit(d)
		}
		order = append(order, a)
	}
	for _, a := range sorted {
		emit(a)
	}
	return order
}

// state is the walk state: the values of the attributes before the
// current position are final.
type state struct {
	values map[string]any
	setBy  map[string]*Rule // the rule that set each derived value
}

func newState() *state {
	return &state{values: map[string]any{}, setBy: map[string]*Rule{}}
}

func (s *state) clone() *state {
	return &state{values: maps.Clone(s.values), setBy: maps.Clone(s.setBy)}
}

// apply applies the rules of position k. A conflict is final: the
// conditions read only final values.
func (c *Configurator) apply(s *state, k int) error {
	for _, rule := range c.rulesAt[k] {
		if !rule.Applies(s.values) {
			continue
		}
		for _, a := range rule.Sets {
			current := s.values[a.Name]
			if current == nil {
				s.values[a.Name] = a.Value
				s.setBy[a.Name] = rule
			} else if current != a.Value {
				attr := c.solver.Model.Attributes[a.Name]
				source := "the answer"
				if r := s.setBy[a.Name]; r != nil {
					source = r.Name()
				}
				return confErrorf("conflict on %s: %s sets %s, but %s set %s",
					a.Name, rule.Name(), attr.Format(a.Value), source, attr.Format(current))
			}
		}
	}
	return nil
}

// forbidden tells if a valid: false rule whose result is final at position
// k matches.
func (c *Configurator) forbidden(s *state, k int) bool {
	for _, rule := range c.forbiddenAt[k] {
		if rule.Applies(s.values) {
			return true
		}
	}
	return false
}

// asked tells if the attribute at position k gets a question (or an only
// allowed value): it is still undefined after its rules, and the sdkconfig
// lines or the chip need it, directly or through rules.
func (c *Configurator) asked(s *state, k int) bool {
	name := c.order[k].Name
	return s.values[name] == nil && c.relevant[name]
}

// feasible tells if the walk from position k can reach an allowed
// configuration: some answers to the next questions give no conflict, no
// valid: false rule and no missing required value.
func (c *Configurator) feasible(s *state, k int) bool {
	s = s.clone()
	for ; k < len(c.order); k++ {
		if c.forbidden(s, k) || c.apply(s, k) != nil {
			return false
		}
		if !c.asked(s, k) {
			continue
		}
		attr := c.order[k]
		for _, v := range c.domain(attr) {
			s.values[attr.Name] = v
			if c.feasible(s, k+1) {
				return true
			}
		}
		return false
	}
	if c.forbidden(s, len(c.order)) {
		return false
	}
	return len(c.solver.FindMissing(s.values, c.required)) == 0
}

// try tells if value v of the attribute at position k can lead to an
// allowed configuration.
func (c *Configurator) try(s *state, k int, v any) bool {
	name := c.order[k].Name
	s.values[name] = v
	ok := c.feasible(s, k+1)
	delete(s.values, name)
	return ok
}

// domain returns the values to try for an attribute: its enum, false and
// true, or for an attribute without enum one value of each class that the
// rules can tell apart.
func (c *Configurator) domain(attr *Attribute) []any {
	if attr.Enum != nil {
		return attr.Enum
	}
	if attr.Type == TypeBoolean {
		return []any{false, true}
	}
	return c.representatives(attr)
}

// representatives returns, for an attribute without enum, a value of each
// set of values that the conditions cannot tell apart: the literals of the
// conditions on the attribute, for a size also 0 and each literal ±1, for
// a string also a value that is none of the literals.
func (c *Configurator) representatives(attr *Attribute) []any {
	var values []any
	addValue := func(v any) {
		if !slices.Contains(values, v) {
			values = append(values, v)
		}
	}
	var walk func(n Node)
	walk = func(n Node) {
		switch x := n.(type) {
		case *Compare:
			if x.Attr == attr {
				addValue(x.Value)
			}
		case *Not:
			walk(x.Operand)
		case *BoolOp:
			for _, o := range x.Operands {
				walk(o)
			}
		}
	}
	for _, rule := range c.solver.Model.Rules {
		if rule.Condition != nil {
			walk(rule.Condition)
		}
	}
	if attr.Type == TypeSize {
		literals := slices.Clone(values)
		addValue(int64(0))
		for _, v := range literals {
			n := v.(int64)
			addValue(n + 1)
			if n > 0 {
				addValue(n - 1)
			}
		}
		sort.Slice(values, func(i, j int) bool { return values[i].(int64) < values[j].(int64) })
		return values
	}
	other := "other"
	for slices.Contains(values, any(other)) {
		other += "_"
	}
	return append(values, other)
}

// Run asks the questions in question order and returns the solution.
//
// For each attribute: a value that a rule sets is shown; an attribute that
// neither the sdkconfig lines nor the chip need is skipped; otherwise the
// allowed answers are the values that can still lead to an allowed
// configuration. One allowed answer is shown without a question. Two or
// more give a question. The default answer is the previous value
// (c.Previous) when it is allowed, else display.default when it is allowed.
//
// An internal attribute (not in the display section) is never shown. It
// can get its value from a rule or as the only allowed value. If it needs a
// question, Run returns an error.
func (c *Configurator) Run(p Prompter) (*Result, error) {
	s := newState()
	if !c.feasible(s, 0) {
		return nil, ErrNoConfiguration
	}
	inputs := map[string]any{}
	var steps []Step
	for k, attr := range c.order {
		// Not possible after a feasible answer; kept as a safety check.
		if c.forbidden(s, k) {
			return nil, ErrNoConfiguration
		}
		if err := c.apply(s, k); err != nil {
			return nil, err
		}
		if v := s.values[attr.Name]; v != nil {
			rule := s.setBy[attr.Name]
			if attr.Visible {
				p.Info(attr, v, c.ruleReason(rule))
			}
			steps = append(steps, Step{attr, v, SourceRule, rule})
			continue
		}
		if !c.asked(s, k) {
			continue
		}

		var value any
		var err error
		source := SourceAnswer
		if attr.Enum == nil && attr.Type != TypeBoolean {
			if !attr.Visible {
				return nil, c.internalError(attr)
			}
			value, err = c.askText(p, s, k)
		} else {
			var options []any
			for _, v := range c.domain(attr) {
				if c.try(s, k, v) {
					options = append(options, v)
				}
			}
			switch len(options) {
			case 0:
				return nil, ErrNoConfiguration // not possible: s is feasible
			case 1:
				value, source = options[0], SourceOnly
				if attr.Visible {
					p.Info(attr, value, "only allowed value")
				}
			default:
				if !attr.Visible {
					return nil, c.internalError(attr)
				}
				q := Question{Attr: attr, Options: options}
				for _, d := range []any{c.Previous[attr.Name], attr.Display.Default} {
					if d != nil && slices.Contains(options, d) {
						q.Default = d
						break
					}
				}
				value, err = ask(p, q)
			}
		}
		if err != nil {
			return nil, err
		}
		s.values[attr.Name] = value
		inputs[attr.Name] = value
		steps = append(steps, Step{Attr: attr, Value: value, Source: source})
	}

	// The same solve as solve.py with these inputs.
	sol, err := c.solver.Evaluate(inputs, nil)
	if err != nil {
		return nil, err
	}
	if !sol.Allowed() {
		return nil, confErrorf("%s", NotAllowedText(sol.Forbidden))
	}
	if missing := c.solver.FindMissing(sol.Values, c.required); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.Name
		}
		return nil, confErrorf("missing value(s): %s", strings.Join(names, ", "))
	}
	return &Result{Inputs: inputs, Solution: sol, Steps: steps}, nil
}

func ask(p Prompter, q Question) (any, error) {
	if q.Attr.Type == TypeBoolean && len(q.Options) == 2 {
		return p.Confirm(q)
	}
	i, err := p.Select(q)
	if err != nil {
		return nil, err
	}
	if i < 0 || i >= len(q.Options) {
		return nil, fmt.Errorf("%s: answer %d is not an option", q.Attr.Name, i)
	}
	return q.Options[i], nil
}

// askText asks the value of an attribute without enum. The answer must
// have the attribute type and lead to an allowed configuration.
func (c *Configurator) askText(p Prompter, s *state, k int) (any, error) {
	attr := c.order[k]
	q := Question{Attr: attr}
	for _, d := range []any{c.Previous[attr.Name], attr.Display.Default} {
		if d != nil && c.try(s, k, d) {
			q.Default = d
			break
		}
	}
	parse := func(text string) (any, error) {
		v, err := attr.Normalize(strings.TrimSpace(text))
		if err != nil {
			return nil, err
		}
		if !c.try(s, k, v) {
			return nil, fmt.Errorf("%s is not allowed with the values above", attr.Label(v))
		}
		return v, nil
	}
	text, err := p.Text(q, func(text string) error {
		_, err := parse(text)
		return err
	})
	if err != nil {
		return nil, err
	}
	return parse(text)
}

// internalError is the error for an internal attribute that needs a
// question: the master configuration must list it in the display section.
func (c *Configurator) internalError(attr *Attribute) error {
	return confErrorf("%s: attribute \"%s\" needs a question, but it is internal (add it to the %s section)",
		c.solver.Model.Path, attr.Name, DisplaySection)
}

// ruleReason returns "set by <titles of the visible attributes in the
// condition>", or "set by a rule" when the condition reads only internal
// attributes.
func (c *Configurator) ruleReason(rule *Rule) string {
	if rule == nil {
		return "set by a rule"
	}
	reads := rule.Reads()
	var titles []string
	for _, a := range c.solver.Model.Order {
		if reads[a.Name] && a.Visible {
			titles = append(titles, a.Title())
		}
	}
	if len(titles) == 0 {
		return "set by a rule"
	}
	return "set by " + strings.Join(titles, ", ")
}
