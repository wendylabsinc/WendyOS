package litebuild

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// BuildProfileOption is the Kconfig option (components/wendy_core/Kconfig in
// wendy-lite) that stores the input values.
const BuildProfileOption = "CONFIG_WENDY_BUILD_PROFILE"

// The attributes that configure.py needs besides the sdkconfig lines (its
// extra_required).
const (
	Chip       = "chip"        // idf.py set-target
	IDFVersion = "idf_version" // the ESP-IDF version, "6.0.3" or "6.1"
	IDFPreview = "idf_preview" // idf.py --preview
)

// Solver applies the rules of a model.
type Solver struct {
	Model *Model
	// Graph maps each node (the attributes and sdkconfig) to the attributes
	// it depends on, in declaration order.
	Graph map[string][]string
	// Order is the evaluation order: each node after its dependencies.
	Order   []string
	rank    map[string]int
	setters []*Rule // the rules that are not valid: false, in evaluation order
}

// NewSolver builds the dependency graph of the model and checks that it
// has no cycle.
func NewSolver(m *Model) (*Solver, error) {
	s := &Solver{Model: m, Graph: buildGraph(m)}
	order, err := topoOrder(m, s.Graph)
	if err != nil {
		return nil, where(m.Path, err)
	}
	s.Order = order
	s.rank = make(map[string]int, len(order))
	for i, node := range order {
		s.rank[node] = i
	}
	// A rule is evaluated at the first of its targets in the graph order.
	// All the attributes in its condition come before, so they are final.
	for _, rule := range m.Rules {
		if !rule.Forbidden {
			s.setters = append(s.setters, rule)
		}
	}
	sort.SliceStable(s.setters, func(i, j int) bool {
		return s.minRank(s.setters[i]) < s.minRank(s.setters[j])
	})
	return s, nil
}

func (s *Solver) minRank(rule *Rule) int {
	best := len(s.Order)
	for _, t := range rule.Targets() {
		best = min(best, s.rank[t])
	}
	return best
}

// buildGraph maps each node (attributes + sdkconfig) to the attributes it
// depends on: each attribute in the condition of a rule that sets it.
func buildGraph(m *Model) map[string][]string {
	deps := map[string]map[string]bool{Sdkconfig: {}}
	for _, a := range m.Order {
		deps[a.Name] = map[string]bool{}
	}
	for _, rule := range m.Rules {
		reads := rule.Reads()
		for _, target := range rule.Targets() {
			for name := range reads {
				deps[target][name] = true
			}
		}
	}
	graph := make(map[string][]string, len(deps))
	for node, set := range deps {
		list := make([]string, 0, len(set))
		for _, a := range m.Order {
			if set[a.Name] {
				list = append(list, a.Name)
			}
		}
		graph[node] = list
	}
	return graph
}

// nodes returns the graph nodes: the attributes in declaration order, then
// sdkconfig.
func nodes(m *Model) []string {
	names := make([]string, 0, len(m.Order)+1)
	for _, a := range m.Order {
		names = append(names, a.Name)
	}
	return append(names, Sdkconfig)
}

// topoOrder returns the nodes so that each node comes after its
// dependencies. Among the nodes that are ready, the declaration order wins.
func topoOrder(m *Model, graph map[string][]string) ([]string, error) {
	all := nodes(m)
	done := map[string]bool{}
	var order []string
	for len(order) < len(all) {
		progress := false
		for _, node := range all {
			if done[node] {
				continue
			}
			ready := true
			for _, dep := range graph[node] {
				if !done[dep] {
					ready = false
					break
				}
			}
			if ready {
				done[node] = true
				order = append(order, node)
				progress = true
				break
			}
		}
		if !progress {
			return nil, confErrorf("dependency cycle: %s", strings.Join(findCycle(all, graph, done), " -> "))
		}
	}
	return order, nil
}

// findCycle returns a cycle among the nodes that are not done, as
// "a -> b -> a" items.
func findCycle(all []string, graph map[string][]string, done map[string]bool) []string {
	state := map[string]int{} // 1: on the path, 2: finished
	var path []string
	var visit func(node string) []string
	visit = func(node string) []string {
		state[node] = 1
		path = append(path, node)
		for _, dep := range graph[node] {
			if done[dep] {
				continue
			}
			if state[dep] == 1 {
				start := slices.Index(path, dep)
				return append(slices.Clone(path[start:]), dep)
			}
			if state[dep] == 0 {
				if cycle := visit(dep); cycle != nil {
					return cycle
				}
			}
		}
		path = path[:len(path)-1]
		state[node] = 2
		return nil
	}
	for _, node := range all {
		if !done[node] && state[node] == 0 {
			if cycle := visit(node); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// Solution is the result of Evaluate.
type Solution struct {
	Values    map[string]any    // nil = undefined
	Sources   map[string]string // who set each defined attribute
	Sdkconfig []string
	Applied   []*Rule // rules with a true condition, file order
	Forbidden []*Rule // valid: false rules with a true condition, file order
}

// Allowed is true when no valid: false rule matches the solved values.
func (s *Solution) Allowed() bool { return len(s.Forbidden) == 0 }

// NotAllowedText returns the error text for the valid: false rules that
// match.
func NotAllowedText(rules []*Rule) string {
	names := make([]string, len(rules))
	for i, r := range rules {
		names[i] = r.Name()
	}
	return "not allowed: " + strings.Join(names, "; ")
}

// Evaluate applies the rules to the input values, in dependency order.
// sources tells where each input value comes from (for the messages); it
// can be nil. A rule that sets a value different from the current value
// gives a ConfError.
func (s *Solver) Evaluate(inputs map[string]any, sources map[string]string) (*Solution, error) {
	sol := &Solution{
		Values:  make(map[string]any, len(s.Model.Order)),
		Sources: map[string]string{},
	}
	for _, a := range s.Model.Order {
		sol.Values[a.Name] = nil
	}
	for name, value := range inputs {
		if value == nil {
			continue
		}
		sol.Values[name] = value
		if src, ok := sources[name]; ok {
			sol.Sources[name] = src
		} else {
			sol.Sources[name] = "input"
		}
	}

	for _, rule := range s.setters {
		if !rule.Applies(sol.Values) {
			continue
		}
		sol.Applied = append(sol.Applied, rule)
		for _, a := range rule.Sets {
			current := sol.Values[a.Name]
			if current == nil {
				sol.Values[a.Name] = a.Value
				sol.Sources[a.Name] = rule.Name()
			} else if current != a.Value {
				attr := s.Model.Attributes[a.Name]
				return nil, confErrorf("conflict on %s: %s sets %s, but %s set %s",
					a.Name, rule.Name(), attr.Format(a.Value), sol.Sources[a.Name], attr.Format(current))
			}
		}
	}

	// A value never changes after it is set, so these are the final values.
	for _, rule := range s.Model.Rules {
		if rule.Forbidden && rule.Applies(sol.Values) {
			sol.Forbidden = append(sol.Forbidden, rule)
		}
	}

	sort.SliceStable(sol.Applied, func(i, j int) bool { return sol.Applied[i].Index < sol.Applied[j].Index })
	for _, rule := range sol.Applied {
		sol.Sdkconfig = append(sol.Sdkconfig, rule.Sdkconfig...)
	}
	return sol, nil
}

// Required returns the attributes that must be defined after the solve,
// mapped to what needs them: the attributes that the sdkconfig lines need,
// plus extra.
func (s *Solver) Required(extra map[string][]string) (map[string][]string, error) {
	required := map[string][]string{}
	for _, name := range s.Graph[Sdkconfig] {
		required[name] = []string{Sdkconfig}
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if s.Model.Attributes[name] == nil {
			return nil, confErrorf("%s needs attribute \"%s\", but %s does not declare it",
				strings.Join(extra[name], ", "), name, s.Model.Path)
		}
		required[name] = append(required[name], extra[name]...)
	}
	return required, nil
}

// Missing is a required attribute that is still undefined.
type Missing struct {
	Name     string
	NeededBy []string // what needs the attribute
	SetBy    []string // attributes whose rules can set it
}

// FindMissing returns each required attribute that is still undefined, in
// graph order.
func (s *Solver) FindMissing(values map[string]any, required map[string][]string) []Missing {
	var missing []Missing
	for _, name := range s.Order {
		if needed, ok := required[name]; ok && values[name] == nil {
			missing = append(missing, Missing{name, needed, s.Graph[name]})
		}
	}
	return missing
}

// --- Inputs and build profile ----------------------------------------------

// ParseAssignments reads NAME=VALUE arguments, as parse_assignments in
// conf_model.py. An empty value (NAME=) gives nil: the attribute is
// undefined.
func (m *Model) ParseAssignments(items []string) (map[string]any, error) {
	values := map[string]any{}
	for _, item := range items {
		name, raw, found := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			return nil, confErrorf("command line: \"%s\": expected NAME=VALUE", item)
		}
		err := func() error {
			if name == Sdkconfig {
				return confErrorf("%s cannot be given on the command line", Sdkconfig)
			}
			attr := m.Attributes[name]
			if attr == nil {
				return confErrorf("unknown attribute \"%s\"", name)
			}
			if _, ok := values[name]; ok {
				return confErrorf("%s is given twice", name)
			}
			raw = strings.TrimSpace(raw)
			if raw == "" {
				values[name] = nil
				return nil
			}
			value, err := attr.Normalize(raw)
			values[name] = value
			return err
		}()
		if err != nil {
			return nil, where("command line: "+item, err)
		}
	}
	return values, nil
}

// Assignments returns the input values as NAME=VALUE arguments, in
// declaration order. solve.py accepts them.
func (m *Model) Assignments(inputs map[string]any) []string {
	var items []string
	for _, a := range m.Order {
		if v := inputs[a.Name]; v != nil {
			items = append(items, a.Name+"="+a.Key(v))
		}
	}
	return items
}

// BuildProfile returns the input values as flat YAML, one "name: value" per
// line, in declaration order, as build_profile in conf_model.py. No inputs
// give "".
func (m *Model) BuildProfile(inputs map[string]any) string {
	var lines []string
	for _, a := range m.Order {
		v := inputs[a.Name]
		if v == nil {
			continue
		}
		var text string
		switch x := v.(type) {
		case bool:
			text = fmt.Sprint(x)
		case int64:
			if s := sizeText(x); strings.ContainsAny(s, "KMG") {
				text = yamlString(s)
			} else {
				text = s
			}
		default:
			text = yamlString(fmt.Sprint(x))
		}
		lines = append(lines, a.Name+": "+text)
	}
	return strings.Join(lines, "\n")
}

// BuildProfileLine returns the sdkconfig line that gives text to
// CONFIG_WENDY_BUILD_PROFILE. Kconfig cannot store a newline, so each
// newline becomes the 2 characters \n. Then the value is escaped as Kconfig
// does: \ becomes \\, then " becomes \".
func BuildProfileLine(text string) string {
	value := strings.ReplaceAll(text, "\n", `\n`)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return BuildProfileOption + `="` + value + `"`
}

// The implicit resolvers of PyYAML (YAML 1.1): a plain string that matches
// one of them is read back as another type, so safe_dump quotes it.
var (
	yamlResolvers = []*regexp.Regexp{
		regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`),
		regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`),
		regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`),
		regexp.MustCompile(`^(?:~|null|Null|NULL|)$`),
		regexp.MustCompile(`^[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?`),
	}
)

// yamlString writes a string as yaml.safe_dump does in a block map: plain
// when YAML reads it back as the same string, else in single quotes. It
// follows the PyYAML rules for printable ASCII text; other text (newlines,
// non-ASCII) is not in a configuration value.
func yamlString(s string) string {
	if yamlPlain(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func yamlPlain(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || strings.HasPrefix(s, "---") || strings.HasPrefix(s, "...") {
		return false
	}
	for _, re := range yamlResolvers {
		if re.MatchString(s) {
			return false
		}
	}
	if strings.ContainsAny(s[:1], "#,[]{}&*!|>'\"%@`") {
		return false
	}
	if strings.ContainsAny(s[:1], "-?:") && (len(s) == 1 || s[1] == ' ') {
		return false
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// SdkconfigText returns the text of sdkconfig.wendy: the header comments,
// the CONFIG_WENDY_BUILD_PROFILE line and the rule lines.
func (m *Model) SdkconfigText(header []string, inputs map[string]any, sol *Solution) string {
	lines := make([]string, 0, len(header)+1+len(sol.Sdkconfig))
	lines = append(lines, header...)
	lines = append(lines, BuildProfileLine(m.BuildProfile(inputs)))
	lines = append(lines, sol.Sdkconfig...)
	return strings.Join(lines, "\n") + "\n"
}
