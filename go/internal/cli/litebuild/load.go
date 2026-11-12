package litebuild

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rule keywords.
const (
	KeywordIf     = "if"
	KeywordElseIf = "else-if"
	KeywordElse   = "else"
)

var keywords = []string{KeywordIf, KeywordElseIf, KeywordElse}

// Assignment is one "name: value" of a rule "set" map.
type Assignment struct {
	Name  string
	Value any
}

// Rule is one item of the rules list.
type Rule struct {
	Index   int
	Keyword string // "if", "else-if", "else", or "" (no condition)
	IfText  string // the condition text as written ("" for else)
	// Condition is nil when the rule has no condition. For else-if and else,
	// it includes the conditions of the chain.
	Condition Node
	Sets      []Assignment
	Sdkconfig []string
	Forbidden bool // valid: false: the condition is not allowed
}

func ruleName(index int, keyword, text string) string {
	name := fmt.Sprintf("rules[%d]", index)
	switch keyword {
	case KeywordElse:
		return name + " (else)"
	case KeywordIf, KeywordElseIf:
		return fmt.Sprintf("%s (%s %s)", name, keyword, text)
	}
	return name
}

// Name returns the rule name of the messages, as in conf_model.py.
func (r *Rule) Name() string { return ruleName(r.Index, r.Keyword, r.IfText) }

// Reads returns the attributes in the condition.
func (r *Rule) Reads() map[string]bool { return nodeAttrs(r.Condition) }

// Targets returns the attributes that the rule sets, then "sdkconfig" when
// it adds lines.
func (r *Rule) Targets() []string {
	targets := make([]string, 0, len(r.Sets)+1)
	for _, a := range r.Sets {
		targets = append(targets, a.Name)
	}
	if len(r.Sdkconfig) > 0 {
		targets = append(targets, Sdkconfig)
	}
	return targets
}

// Applies tells if the condition is true for the values.
func (r *Rule) Applies(values map[string]any) bool {
	return r.Condition == nil || r.Condition.Eval(values)
}

// Model is a master configuration.
type Model struct {
	Path          string
	Attributes    map[string]*Attribute
	Order         []*Attribute // declaration order
	Rules         []*Rule
	FormatVersion string
}

// LoadFile reads and checks a master configuration file.
func LoadFile(path string) (*Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data, path)
}

// Load reads and checks a master configuration. name is used in the
// messages.
func Load(data []byte, name string) (*Model, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, confErrorf("%s: %v", name, err)
	}
	m, err := load(&doc)
	if err != nil {
		return nil, where(name, err)
	}
	m.Path = name
	return m, nil
}

func load(doc *yaml.Node) (*Model, error) {
	root := documentRoot(doc)
	data, err := mapping(root, "expected a map with attributes and rules")
	if err != nil {
		return nil, err
	}
	if err := checkKeys(data, "format_version", "attributes", "rules", DisplaySection); err != nil {
		return nil, err
	}
	m := &Model{}
	if raw := data.get("format_version"); raw != nil {
		m.FormatVersion = formatVersionText(raw)
		if !slices.Contains(FormatVersions, m.FormatVersion) {
			supported := make([]string, len(FormatVersions))
			for i, v := range FormatVersions {
				supported[i] = `"` + v + `"`
			}
			return nil, confErrorf("format_version %s is not supported (supported: %s)",
				pyRepr(scalarValue(raw)), strings.Join(supported, ", "))
		}
	}
	attrsNode := data.get("attributes")
	if attrsNode == nil {
		return nil, confErrorf("attributes is missing")
	}
	if err := m.parseAttributes(attrsNode); err != nil {
		return nil, err
	}
	if rulesNode := data.get("rules"); rulesNode != nil && !isNull(rulesNode) {
		if rulesNode.Kind != yaml.SequenceNode {
			return nil, confErrorf("rules must be a list")
		}
		var chain []Node
		for i, n := range rulesNode.Content {
			rule, newChain, err := m.parseRule(i, resolve(n), chain)
			if err != nil {
				return nil, err
			}
			m.Rules = append(m.Rules, rule)
			chain = newChain
		}
	}
	if displayNode := data.get(DisplaySection); displayNode != nil && !isNull(displayNode) {
		if err := m.parseDisplay(displayNode); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Model) parseAttributes(node *yaml.Node) error {
	entries, err := mapping(node, "attributes must be a map of 'name: {type, enum}' entries")
	if err != nil {
		return err
	}
	m.Attributes = map[string]*Attribute{}
	for _, e := range entries {
		name := e.key.Value
		err := func() error {
			if e.key.ShortTag() != "!!str" || !nameRe.MatchString(name) {
				return confErrorf("not a valid attribute name")
			}
			if name == Sdkconfig {
				return confErrorf("%s is intrinsic, it cannot be declared", Sdkconfig)
			}
			if name == "true" || name == "false" {
				return confErrorf("%s is a reserved word", name)
			}
			spec, err := mapping(e.value, "expected a map with type and enum")
			if err != nil {
				return err
			}
			if err := checkKeys(spec, "type", "enum"); err != nil {
				return err
			}
			typ := ""
			if t := spec.get("type"); t != nil && t.ShortTag() == "!!str" {
				typ = t.Value
			}
			if !slices.Contains(types, typ) {
				return confErrorf("type must be one of: %s", strings.Join(types, ", "))
			}
			attr := &Attribute{Name: name, Type: typ, Index: len(m.Order)}
			if enumNode := spec.get("enum"); enumNode != nil {
				if typ == TypeBoolean {
					return confErrorf("a boolean has no enum")
				}
				if enumNode.Kind != yaml.SequenceNode || len(enumNode.Content) == 0 {
					return confErrorf("enum must be a list with one or more values")
				}
				for _, v := range enumNode.Content {
					value, err := attr.Convert(scalarValue(resolve(v)))
					if err != nil {
						return err
					}
					attr.Enum = append(attr.Enum, value)
				}
			}
			m.Attributes[name] = attr
			m.Order = append(m.Order, attr)
			return nil
		}()
		if err != nil {
			return where("attributes: "+name, err)
		}
	}
	return nil
}

// parseRule reads one rule. chain holds the conditions of the open
// if/else-if chain, or nil when no chain is open. It returns the rule and
// the chain after it.
func (m *Model) parseRule(index int, node *yaml.Node, chain []Node) (*Rule, []Node, error) {
	data, err := mapping(node, fmt.Sprintf("rules[%d]: expected a map with if, else-if or else, set, %s or valid", index, Sdkconfig))
	if err != nil {
		return nil, nil, err
	}
	var found []string
	for _, k := range keywords {
		if data.get(k) != nil {
			found = append(found, k)
		}
	}
	keyword, ifText := "", ""
	if len(found) > 0 {
		keyword = found[0]
	}
	textIsString := false
	if keyword == KeywordIf || keyword == KeywordElseIf {
		if n := data.get(keyword); n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" {
			ifText, textIsString = n.Value, true
		}
	}
	name := ruleName(index, keyword, ifText)
	if (keyword == KeywordIf || keyword == KeywordElseIf) && !textIsString {
		name = fmt.Sprintf("rules[%d]", index)
	}

	rule, newChain, err := func() (*Rule, []Node, error) {
		if err := checkKeys(data, KeywordIf, KeywordElseIf, KeywordElse, "set", Sdkconfig, "valid"); err != nil {
			return nil, nil, err
		}
		if len(found) > 1 {
			return nil, nil, confErrorf("use only one of if, else-if and else")
		}
		if (keyword == KeywordElseIf || keyword == KeywordElse) && chain == nil {
			return nil, nil, confErrorf("%s must follow an if or else-if rule", keyword)
		}

		var condition Node
		var newChain []Node
		switch keyword {
		case KeywordIf, KeywordElseIf:
			if !textIsString || strings.TrimSpace(ifText) == "" {
				return nil, nil, confErrorf("%s must be a condition text", keyword)
			}
			own, err := ParseCondition(ifText, m.Attributes)
			if err != nil {
				return nil, nil, err
			}
			if keyword == KeywordIf {
				condition, newChain = own, []Node{own}
			} else {
				operands := make([]Node, 0, len(chain)+1)
				for _, c := range chain {
					operands = append(operands, &Not{Operand: c})
				}
				condition = all(append(operands, own))
				newChain = append(slices.Clone(chain), own)
			}
		case KeywordElse:
			if !isNull(data.get(KeywordElse)) {
				return nil, nil, confErrorf("else takes no condition (write else: alone, or use else-if)")
			}
			operands := make([]Node, 0, len(chain))
			for _, c := range chain {
				operands = append(operands, &Not{Operand: c})
			}
			condition = all(operands)
		}

		if valid := data.get("valid"); valid != nil {
			if v, ok := scalarValue(valid).(bool); !ok || v {
				return nil, nil, confErrorf("valid must be false (leave it out for an allowed combination)")
			}
			if condition == nil {
				return nil, nil, confErrorf("valid: false needs an if, else-if or else")
			}
			if data.get("set") != nil || data.get(Sdkconfig) != nil {
				return nil, nil, confErrorf("valid: false cannot have set or %s", Sdkconfig)
			}
			return &Rule{Index: index, Keyword: keyword, IfText: ifText, Condition: condition, Forbidden: true}, newChain, nil
		}

		var sets []Assignment
		if setNode := data.get("set"); setNode != nil {
			entries, err := mapping(setNode, "set must be a map of 'name: value' entries")
			if err != nil {
				return nil, nil, err
			}
			for _, e := range entries {
				attrName := e.key.Value
				if attrName == Sdkconfig {
					return nil, nil, confErrorf("set: use the %s key to add %s lines", Sdkconfig, Sdkconfig)
				}
				attr := m.Attributes[attrName]
				if attr == nil {
					return nil, nil, confErrorf("set: unknown attribute \"%s\"", attrName)
				}
				raw := scalarValue(e.value)
				if raw == nil {
					return nil, nil, confErrorf("set: %s has no value", attrName)
				}
				value, err := attr.Normalize(raw)
				if err != nil {
					return nil, nil, err
				}
				sets = append(sets, Assignment{attrName, value})
			}
		}

		var lines []string
		if n := data.get(Sdkconfig); n != nil {
			lines, err = parseSdkconfig(n)
			if err != nil {
				return nil, nil, err
			}
		}
		if len(sets) == 0 && len(lines) == 0 {
			return nil, nil, confErrorf("the rule sets nothing (add set or %s)", Sdkconfig)
		}
		return &Rule{Index: index, Keyword: keyword, IfText: ifText, Condition: condition, Sets: sets, Sdkconfig: lines}, newChain, nil
	}()
	if err != nil {
		return nil, nil, where(name, err)
	}
	return rule, newChain, nil
}

func all(operands []Node) Node {
	if len(operands) == 1 {
		return operands[0]
	}
	return &BoolOp{Op: "&&", Operands: operands}
}

func parseSdkconfig(node *yaml.Node) ([]string, error) {
	var chunks []string
	switch {
	case node.Kind == yaml.ScalarNode && node.ShortTag() == "!!str":
		chunks = []string{node.Value}
	case node.Kind == yaml.SequenceNode:
		for _, item := range node.Content {
			item = resolve(item)
			if item.Kind != yaml.ScalarNode || item.ShortTag() != "!!str" {
				return nil, confErrorf("%s must be a text block or a list of lines", Sdkconfig)
			}
			chunks = append(chunks, item.Value)
		}
	default:
		return nil, confErrorf("%s must be a text block or a list of lines", Sdkconfig)
	}
	var lines []string
	for _, chunk := range chunks {
		chunk = strings.ReplaceAll(chunk, "\r\n", "\n")
		for _, line := range strings.FieldsFunc(chunk, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines, nil
}

// parseDisplay reads the display section. The Python solver ignores it.
func (m *Model) parseDisplay(node *yaml.Node) error {
	entries, err := mapping(node, "display must be a map of 'name: {title, help, priority, default, values}' entries")
	if err != nil {
		return where(DisplaySection, err)
	}
	for _, e := range entries {
		name := e.key.Value
		err := func() error {
			attr := m.Attributes[name]
			if attr == nil {
				return confErrorf("unknown attribute")
			}
			if isNull(e.value) {
				attr.Visible = true // "name:" alone: visible, no display keys
				return nil
			}
			spec, err := mapping(e.value, "expected a map with title, help, priority, default and values")
			if err != nil {
				return err
			}
			if err := checkKeys(spec, "title", "help", "priority", "default", "values"); err != nil {
				return err
			}
			attr.Visible = true
			d := &attr.Display
			if d.Title, err = optionalString(spec, "title"); err != nil {
				return err
			}
			if d.Help, err = optionalString(spec, "help"); err != nil {
				return err
			}
			if n := spec.get("priority"); n != nil {
				p, ok := scalarValue(n).(int64)
				if !ok {
					return confErrorf("priority must be an integer")
				}
				pi := int(p)
				d.Priority = &pi
			}
			if n := spec.get("default"); n != nil {
				value, err := attr.Normalize(scalarValue(n))
				if err != nil {
					return where("default", err)
				}
				d.Default = value
			}
			if n := spec.get("values"); n != nil {
				labels, err := mapping(n, "values must be a map of 'value: label' entries")
				if err != nil {
					return err
				}
				d.Labels = map[string]string{}
				for _, l := range labels {
					value, err := attr.Normalize(scalarValue(l.key))
					if err != nil {
						return where("values", err)
					}
					if l.value.Kind != yaml.ScalarNode || l.value.ShortTag() != "!!str" {
						return confErrorf("values: the label of %s must be a text", attr.Key(value))
					}
					d.Labels[attr.Key(value)] = l.value.Value
				}
			}
			return nil
		}()
		if err != nil {
			return where(DisplaySection+": "+name, err)
		}
	}
	return nil
}

func optionalString(spec mapNode, key string) (string, error) {
	n := spec.get(key)
	if n == nil {
		return "", nil
	}
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return "", confErrorf("%s must be a text", key)
	}
	return n.Value, nil
}

// --- YAML helpers ---------------------------------------------------------

type mapEntry struct {
	key, value *yaml.Node
}

// mapNode is a YAML map, in file order.
type mapNode []mapEntry

func (m mapNode) get(key string) *yaml.Node {
	for _, e := range m {
		if e.key.Value == key {
			return e.value
		}
	}
	return nil
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return resolve(doc.Content[0])
	}
	return resolve(doc)
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	n = resolve(n)
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

// mapping returns the entries of a YAML map. A key given twice is an error,
// as with _UniqueKeyLoader in conf_model.py.
func mapping(n *yaml.Node, message string) (mapNode, error) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, confErrorf("%s", message)
	}
	entries := make(mapNode, 0, len(n.Content)/2)
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := resolve(n.Content[i]), resolve(n.Content[i+1])
		if key.Kind == yaml.ScalarNode {
			if seen[key.Value] {
				return nil, confErrorf("line %d: %s is given twice", key.Line, pyRepr(scalarValue(key)))
			}
			seen[key.Value] = true
		}
		entries = append(entries, mapEntry{key, value})
	}
	return entries, nil
}

func checkKeys(m mapNode, allowed ...string) error {
	var unknown []string
	for _, e := range m {
		if !slices.Contains(allowed, e.key.Value) {
			unknown = append(unknown, e.key.Value)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return confErrorf("unknown key(s): %s", strings.Join(unknown, ", "))
}

// scalarValue returns the value of a YAML scalar: a string, an int64, a
// float64, a bool or nil.
func scalarValue(n *yaml.Node) any {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil
	}
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return f
		}
	}
	return n.Value
}

// formatVersionText returns format_version as str() does in Python: a
// string, an int or a float, not a bool.
func formatVersionText(n *yaml.Node) string {
	switch v := scalarValue(n).(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	return ""
}
