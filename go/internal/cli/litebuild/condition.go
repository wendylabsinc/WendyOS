package litebuild

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Node is a rule condition.
type Node interface {
	// Attrs adds the names of the attributes that the condition reads.
	Attrs(set map[string]bool)
	// Eval evaluates the condition. A missing or nil value is undefined.
	Eval(values map[string]any) bool
	String() string
}

// nodeAttrs returns the attributes that a condition reads.
func nodeAttrs(n Node) map[string]bool {
	set := map[string]bool{}
	if n != nil {
		n.Attrs(set)
	}
	return set
}

var compareOps = []string{"==", "!=", "<", "<=", ">", ">="}

func isCompareOp(op string) bool { return slices.Contains(compareOps, op) }

func isOrderingOp(op string) bool { return op == "<" || op == "<=" || op == ">" || op == ">=" }

// Compare is "attr op literal".
type Compare struct {
	Attr  *Attribute
	Op    string
	Value any
}

func (c *Compare) Attrs(set map[string]bool) { set[c.Attr.Name] = true }

func (c *Compare) Eval(values map[string]any) bool {
	actual := values[c.Attr.Name]
	if actual == nil {
		// An undefined attribute is equal to no value.
		return c.Op == "!="
	}
	switch c.Op {
	case "==":
		return actual == c.Value
	case "!=":
		return actual != c.Value
	}
	a, aok := actual.(int64)
	b, bok := c.Value.(int64)
	if !aok || !bok {
		return false
	}
	switch c.Op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	default:
		return a >= b
	}
}

func (c *Compare) String() string {
	literal := c.Attr.Format(c.Value)
	if c.Attr.Type == TypeString {
		literal = `"` + fmt.Sprint(c.Value) + `"`
	}
	return fmt.Sprintf("%s %s %s", c.Attr.Name, c.Op, literal)
}

// BoolTest is a boolean attribute alone: true only if the value is true.
type BoolTest struct {
	Attr *Attribute
}

func (b *BoolTest) Attrs(set map[string]bool) { set[b.Attr.Name] = true }

func (b *BoolTest) Eval(values map[string]any) bool { return values[b.Attr.Name] == true }

func (b *BoolTest) String() string { return b.Attr.Name }

// Not is "!operand".
type Not struct {
	Operand Node
}

func (n *Not) Attrs(set map[string]bool) { n.Operand.Attrs(set) }

func (n *Not) Eval(values map[string]any) bool { return !n.Operand.Eval(values) }

func (n *Not) String() string {
	switch n.Operand.(type) {
	case *Not, *BoolTest:
		return "!" + n.Operand.String()
	}
	return "!(" + n.Operand.String() + ")"
}

// BoolOp is "a && b && ..." or "a || b || ...".
type BoolOp struct {
	Op       string // "&&" or "||"
	Operands []Node
}

func (b *BoolOp) Attrs(set map[string]bool) {
	for _, o := range b.Operands {
		o.Attrs(set)
	}
}

func (b *BoolOp) Eval(values map[string]any) bool {
	for _, o := range b.Operands {
		r := o.Eval(values)
		if b.Op == "&&" && !r {
			return false
		}
		if b.Op == "||" && r {
			return true
		}
	}
	return b.Op == "&&"
}

func (b *BoolOp) String() string {
	parts := make([]string, len(b.Operands))
	for i, o := range b.Operands {
		if _, ok := o.(*BoolOp); ok {
			parts[i] = "(" + o.String() + ")"
		} else {
			parts[i] = o.String()
		}
	}
	return strings.Join(parts, " "+b.Op+" ")
}

// --- Parser ---------------------------------------------------------------

var tokenRe = regexp.MustCompile(`^(?:` +
	`(==|!=|<=|>=|&&|\|\||[<>!()])` +
	`|("[^"]*"|'[^']*')` +
	`|(\d+(?:\s*(?:KiB|MiB|GiB))?)` +
	`|([A-Za-z_][A-Za-z0-9_]*)` +
	`)`)

var tokenKinds = []string{"op", "string", "number", "name"}

type token struct {
	kind string // op, string, number, name or end
	text string
	col  int // 1-based
}

func tokenize(text string) ([]token, error) {
	var tokens []token
	pos := 0
	for {
		for pos < len(text) && unicode.IsSpace(rune(text[pos])) {
			pos++
		}
		if pos == len(text) {
			return append(tokens, token{"end", "", pos + 1}), nil
		}
		m := tokenRe.FindStringSubmatchIndex(text[pos:])
		if m == nil {
			return nil, confErrorf("unexpected \"%s\" at column %d", string(text[pos]), pos+1)
		}
		kind := ""
		for i, k := range tokenKinds {
			if m[2*(i+1)] >= 0 {
				kind = k
				break
			}
		}
		tokens = append(tokens, token{kind, text[pos : pos+m[1]], pos + 1})
		pos += m[1]
	}
}

// parser is a recursive-descent parser for a rule condition.
//
//	expr    := and ('||' and)*
//	and     := unary ('&&' unary)*
//	unary   := '!' unary | '(' expr ')' | compare
//	compare := NAME op LITERAL     op: == != < <= > >=
//	         | NAME                a boolean attribute alone
//	LITERAL := "string" | size such as 8MiB | true | false
type parser struct {
	tokens     []token
	i          int
	attributes map[string]*Attribute
}

// ParseCondition parses a rule condition.
func ParseCondition(text string, attributes map[string]*Attribute) (Node, error) {
	tokens, err := tokenize(text)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens, attributes: attributes}
	node, err := p.expr()
	if err != nil {
		return nil, err
	}
	if tok := p.peek(); tok.kind != "end" {
		return nil, p.errorf(tok, "expected && or || or the end")
	}
	return node, nil
}

func (p *parser) peek() token { return p.tokens[p.i] }

func (p *parser) take() token {
	tok := p.tokens[p.i]
	if tok.kind != "end" {
		p.i++
	}
	return tok
}

func (p *parser) isOp(text string) bool {
	tok := p.peek()
	return tok.kind == "op" && tok.text == text
}

func (p *parser) errorf(tok token, message string) error {
	found := "the end"
	if tok.kind != "end" {
		found = fmt.Sprintf("\"%s\" at column %d", tok.text, tok.col)
	}
	return confErrorf("%s, found %s", message, found)
}

func (p *parser) expr() (Node, error) { return p.boolOp("||", p.and) }

func (p *parser) and() (Node, error) { return p.boolOp("&&", p.unary) }

func (p *parser) boolOp(op string, operand func() (Node, error)) (Node, error) {
	first, err := operand()
	if err != nil {
		return nil, err
	}
	operands := []Node{first}
	for p.isOp(op) {
		p.take()
		next, err := operand()
		if err != nil {
			return nil, err
		}
		operands = append(operands, next)
	}
	if len(operands) == 1 {
		return first, nil
	}
	return &BoolOp{Op: op, Operands: operands}, nil
}

func (p *parser) unary() (Node, error) {
	if p.isOp("!") {
		p.take()
		operand, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &Not{Operand: operand}, nil
	}
	if p.isOp("(") {
		p.take()
		node, err := p.expr()
		if err != nil {
			return nil, err
		}
		if !p.isOp(")") {
			return nil, p.errorf(p.peek(), "expected )")
		}
		p.take()
		return node, nil
	}
	return p.compare()
}

func (p *parser) compare() (Node, error) {
	tok := p.take()
	if tok.kind != "name" {
		return nil, p.errorf(tok, "expected an attribute name")
	}
	if tok.text == Sdkconfig {
		return nil, confErrorf("%s cannot be used in a condition", Sdkconfig)
	}
	attr := p.attributes[tok.text]
	if attr == nil {
		return nil, confErrorf("unknown attribute \"%s\" at column %d", tok.text, tok.col)
	}

	next := p.peek()
	if attr.Type == TypeBoolean && !(next.kind == "op" && isCompareOp(next.text)) {
		return &BoolTest{Attr: attr}, nil
	}

	op := p.take()
	if op.kind != "op" || !isCompareOp(op.text) {
		return nil, p.errorf(op, "expected a comparison after "+attr.Name)
	}
	if isOrderingOp(op.text) && attr.Type != TypeSize {
		return nil, confErrorf("%s needs a size attribute, %s is a %s", op.text, attr.Name, attr.Type)
	}

	lit := p.take()
	var raw any
	switch attr.Type {
	case TypeString:
		if lit.kind != "string" {
			return nil, p.errorf(lit, fmt.Sprintf("expected a quoted string after %s %s", attr.Name, op.text))
		}
		raw = lit.text[1 : len(lit.text)-1]
	case TypeBoolean:
		if lit.kind != "name" || (lit.text != "true" && lit.text != "false") {
			return nil, p.errorf(lit, fmt.Sprintf("expected true or false after %s %s", attr.Name, op.text))
		}
		raw = lit.text == "true"
	default:
		if lit.kind != "number" {
			return nil, p.errorf(lit, fmt.Sprintf("expected a size such as 8MiB after %s %s", attr.Name, op.text))
		}
		raw = lit.text
	}
	// == and != also check the enum, so a typo fails at load time.
	var value any
	var err error
	if op.text == "==" || op.text == "!=" {
		value, err = attr.Normalize(raw)
	} else {
		value, err = attr.Convert(raw)
	}
	if err != nil {
		return nil, err
	}
	return &Compare{Attr: attr, Op: op.text, Value: value}, nil
}
