package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// ParseError is an error in the text form of a query. It wraps ErrInvalid.
type ParseError struct {
	Pos   int    // byte offset of the offending token; len(text) at end of input
	Token string // the offending token, empty at end of input
	Msg   string
}

func (e *ParseError) Error() string {
	if e.Token == "" {
		return fmt.Sprintf("query: %s at offset %d", e.Msg, e.Pos)
	}
	return fmt.Sprintf("query: %s at offset %d near %q", e.Msg, e.Pos, e.Token)
}

func (e *ParseError) Unwrap() error { return ErrInvalid }

type token struct {
	pos int
	raw string
}

func (t token) fail(format string, args ...any) *ParseError {
	return &ParseError{Pos: t.pos, Token: t.raw, Msg: fmt.Sprintf(format, args...)}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// lex splits text at white space. A double-quoted segment (Go string
// syntax) may hold white space and commas.
func lex(text string) ([]token, *ParseError) {
	var out []token
	for i := 0; i < len(text); {
		if isSpace(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && !isSpace(text[i]) {
			if text[i] == '"' {
				for i++; i < len(text) && text[i] != '"'; i++ {
					if text[i] == '\\' {
						i++
					}
				}
				if i >= len(text) {
					return nil, &ParseError{Pos: start, Token: text[start:], Msg: "unterminated quote"}
				}
			}
			i++
		}
		out = append(out, token{start, text[start:i]})
	}
	return out, nil
}

// splitList splits at the commas outside quotes.
func splitList(raw string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '\\' && quoted:
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	return append(out, raw[start:])
}

// word decodes one value: a bare word, or one quoted string.
func word(raw string) (string, bool) {
	if strings.HasPrefix(raw, `"`) {
		s, err := strconv.Unquote(raw)
		return s, err == nil
	}
	return raw, raw != "" && !strings.Contains(raw, `"`)
}

// quote writes a value so that word reads it back.
func quote(s string) string {
	if s == "" || s[0] == '@' {
		return strconv.Quote(s)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' || c == '"' || c == ',' || c == '\\' {
			return strconv.Quote(s)
		}
	}
	return s
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func parseFloats(raw string, n int) ([]float64, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) != n {
		return nil, false
	}
	out := make([]float64, n)
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || !finite(v) {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

type parser struct {
	text string
	toks []token
	i    int
	now  time.Time
	seen map[string]bool
}

func (p *parser) next(what string) (token, *ParseError) {
	if p.i >= len(p.toks) {
		return token{}, &ParseError{Pos: len(p.text), Msg: "expected " + what}
	}
	p.i++
	return p.toks[p.i-1], nil
}

func (p *parser) once(name string, t token) *ParseError {
	if p.seen[name] {
		return t.fail("duplicate %s", name)
	}
	p.seen[name] = true
	return nil
}

func (p *parser) count(what string) (int, *ParseError) {
	t, err := p.next(what)
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(t.raw)
	if convErr != nil || n < 1 {
		return 0, t.fail("expected %s", what)
	}
	return n, nil
}

func (p *parser) distance() (float64, *ParseError) {
	t, err := p.next("a distance such as 5m")
	if err != nil {
		return 0, err
	}
	v, ok := parseFloats(strings.TrimSuffix(t.raw, "m"), 1)
	if !ok || v[0] <= 0 {
		return 0, t.fail("expected a positive distance such as 5m")
	}
	return v[0], nil
}

func (p *parser) name(what string) (string, *ParseError) {
	t, err := p.next(what)
	if err != nil {
		return "", err
	}
	s, ok := word(t.raw)
	if !ok || s == "" {
		return "", t.fail("expected %s", what)
	}
	return s, nil
}

func (p *parser) ref() (Ref, *ParseError) {
	const what = "an entity id or a point @x,z[:floor]"
	t, err := p.next(what)
	if err != nil {
		return Ref{}, err
	}
	if !strings.HasPrefix(t.raw, "@") {
		s, ok := word(t.raw)
		if !ok || s == "" {
			return Ref{}, t.fail("expected %s", what)
		}
		return Ref{Entity: s}, nil
	}
	coords, floor, hasFloor := strings.Cut(t.raw[1:], ":")
	v, ok := parseFloats(coords, 2)
	if !ok {
		return Ref{}, t.fail("expected a point @x,z[:floor]")
	}
	r := Ref{Point: &geom.Vec2{X: v[0], Z: v[1]}}
	if hasFloor {
		f, ok := word(floor)
		if !ok || f == "" {
			return Ref{}, t.fail("expected a floor after ':'")
		}
		r.Floor = spatial.FloorID(f)
	}
	return r, nil
}

func (p *parser) instant() (time.Time, *ParseError) {
	const what = "a time: RFC 3339, now, or now-<duration>"
	t, err := p.next(what)
	if err != nil {
		return time.Time{}, err
	}
	if rest, ok := strings.CutPrefix(t.raw, "now"); ok {
		var ago time.Duration
		if rest != "" {
			d, convErr := time.ParseDuration(strings.TrimPrefix(rest, "-"))
			if convErr != nil || !strings.HasPrefix(rest, "-") || d < 0 {
				return time.Time{}, t.fail("expected %s", what)
			}
			ago = d
		}
		if p.now.IsZero() {
			return time.Time{}, t.fail("a relative time needs the current time, which the caller did not supply")
		}
		return p.now.Add(-ago).UTC(), nil
	}
	at, convErr := time.Parse(time.RFC3339Nano, t.raw)
	if convErr != nil || at.IsZero() {
		return time.Time{}, t.fail("expected %s", what)
	}
	return at.UTC(), nil
}

// Parse reads the text form of a query:
//
//	query    = ( "find" | "nearest" K ) { clause }
//	clause   = "kind=" KIND{","KIND} | "class=" CLASS{","CLASS} | "on" FLOOR
//	         | ( "to" | "from" ) REF | "within" DIST | "visible" | "path" DIST
//	         | "in" "box" X0,Z0,X1,Z1 | "in" "region" NAME
//	         | "in" "polygon" X,Z X,Z X,Z { X,Z }
//	         | "near" REF DIST | "same-region" REF | "visible-from" REF | "inside" REF
//	         | "since" TIME | "before" TIME | "at" TIME
//	         | "order" ( "distance" | "path" | "time" | "key" ) [ "asc" | "desc" ]
//	         | "limit" N
//	REF      = ENTITY | "@" X "," Z [ ":" FLOOR ]
//	DIST     = NUMBER [ "m" ]
//	TIME     = RFC 3339 | "now" | "now-" DURATION
//
// Words are separated by white space; a value with white space, a comma or a
// leading '@' is written as a Go quoted string (class="office chair"). "to"
// names the origin that nearest, within, visible, path and the distance
// orders refer to. Each clause may appear once, except the relations.
// "now" is the caller's clock, passed as now; a zero now makes relative times
// an error. A syntax error is a *ParseError carrying the byte offset.
func Parse(text string, now time.Time) (Query, error) {
	toks, lexErr := lex(text)
	if lexErr != nil {
		return Query{}, lexErr
	}
	p := &parser{text: text, toks: toks, now: now, seen: map[string]bool{}}
	q, err := p.query()
	if err != nil {
		return Query{}, err
	}
	if err := q.Validate(); err != nil {
		return Query{}, &ParseError{Pos: 0, Token: text, Msg: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")}
	}
	return q, nil
}

func (p *parser) query() (Query, *ParseError) {
	var q Query
	var needsOrigin *token // first clause that refers to the origin
	verb, err := p.next(`"find" or "nearest K"`)
	if err != nil {
		return q, err
	}
	switch verb.raw {
	case "find":
	case "nearest":
		if q.Nearest, err = p.count("a count of at least 1"); err != nil {
			return q, err
		}
		needsOrigin = &verb
	default:
		return q, verb.fail(`a query starts with "find" or "nearest K"`)
	}
	for p.i < len(p.toks) {
		t, _ := p.next("")
		key, value := t.raw, ""
		if k, v, ok := strings.Cut(t.raw, "="); ok && (k == "kind" || k == "class") {
			key, value = k+"=", v
		}
		if key == "from" {
			key = "to"
		}
		relation := key == "near" || key == "same-region" || key == "visible-from" || key == "inside"
		if key == "in" {
			sub, err := p.next(`"box", "region" or "polygon"`)
			if err != nil {
				return q, err
			}
			key, t = "in "+sub.raw, sub
		}
		if !relation {
			if err := p.once(key, t); err != nil {
				return q, err
			}
		}
		if (key == "within" || key == "visible" || key == "path") && needsOrigin == nil {
			needsOrigin = &t
		}
		switch key {
		case "kind=":
			for _, item := range splitList(value) {
				k := spatial.EntityKind(item)
				if !k.Valid() {
					return q, t.fail("unknown entity kind %q", item)
				}
				q.Kinds = append(q.Kinds, k)
			}
		case "class=":
			for _, item := range splitList(value) {
				s, ok := word(item)
				if !ok {
					return q, t.fail("expected class=NAME[,NAME]")
				}
				q.Classes = append(q.Classes, s)
			}
		case "on":
			f, err := p.name("a floor")
			if err != nil {
				return q, err
			}
			q.Floor = spatial.FloorID(f)
		case "to":
			r, err := p.ref()
			if err != nil {
				return q, err
			}
			q.Origin = &r
		case "within":
			if q.WithinM, err = p.distance(); err != nil {
				return q, err
			}
		case "visible":
			q.Visible = true
		case "path":
			if q.PathWithinM, err = p.distance(); err != nil {
				return q, err
			}
		case "in box":
			b, err := p.next("a box x0,z0,x1,z1")
			if err != nil {
				return q, err
			}
			v, ok := parseFloats(b.raw, 4)
			if !ok || v[0] > v[2] || v[1] > v[3] {
				return q, b.fail("expected a box x0,z0,x1,z1 with x0 <= x1 and z0 <= z1")
			}
			q.Box = &Box{Min: geom.Vec2{X: v[0], Z: v[1]}, Max: geom.Vec2{X: v[2], Z: v[3]}}
		case "in region":
			if q.Region, err = p.name("a region id or label"); err != nil {
				return q, err
			}
		case "in polygon":
			for p.i < len(p.toks) {
				v, ok := parseFloats(p.toks[p.i].raw, 2)
				if !ok {
					break
				}
				q.Polygon = append(q.Polygon, geom.Vec2{X: v[0], Z: v[1]})
				p.i++
			}
			if n := len(q.Polygon); n < 3 || n > spatial.MaxPolygonVertices {
				return q, t.fail("a polygon needs 3 to %d vertices written x,z", spatial.MaxPolygonVertices)
			}
		case "near", "same-region", "visible-from", "inside":
			rel := Relation{Kind: RelationKind(strings.ReplaceAll(key, "-", "_"))}
			if rel.To, err = p.ref(); err != nil {
				return q, err
			}
			if key == "near" {
				if rel.DistanceM, err = p.distance(); err != nil {
					return q, err
				}
			}
			q.Relations = append(q.Relations, rel)
		case "since":
			if q.SeenSince, err = p.instant(); err != nil {
				return q, err
			}
		case "before":
			if q.SeenBefore, err = p.instant(); err != nil {
				return q, err
			}
		case "at":
			if q.ValidAt, err = p.instant(); err != nil {
				return q, err
			}
		case "order":
			o, err := p.next("an order: distance, path, time or key")
			if err != nil {
				return q, err
			}
			switch q.Order = Order(o.raw); q.Order {
			case OrderDistance, OrderPath:
				if needsOrigin == nil {
					needsOrigin = &o
				}
			case OrderTime, OrderKey:
			default:
				return q, o.fail("expected an order: distance, path, time or key")
			}
			if p.i < len(p.toks) && (p.toks[p.i].raw == "asc" || p.toks[p.i].raw == "desc") {
				q.Descending = p.toks[p.i].raw == "desc"
				p.i++
			}
		case "limit":
			if q.Limit, err = p.count("a limit of at least 1"); err != nil {
				return q, err
			}
		default:
			return q, t.fail("unknown clause")
		}
	}
	if q.Origin == nil && needsOrigin != nil {
		return q, needsOrigin.fail(`this needs an origin: add "to <entity>" or "to @x,z"`)
	}
	return q, nil
}

func (r Ref) text() string {
	if r.Point == nil {
		return quote(r.Entity)
	}
	s := "@" + num(r.Point.X) + "," + num(r.Point.Z)
	if r.Floor != "" {
		s += ":" + quote(string(r.Floor))
	}
	return s
}

func instantText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// String writes the text form: Parse(q.String(), now) returns a query equal
// to q for every valid q whose zero-valued bounds mean "none".
func (q Query) String() string {
	parts := []string{"find"}
	if q.Nearest > 0 {
		parts = []string{"nearest", strconv.Itoa(q.Nearest)}
	}
	add := func(s ...string) { parts = append(parts, s...) }
	if len(q.Kinds) > 0 {
		kinds := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			kinds[i] = string(k)
		}
		add("kind=" + strings.Join(kinds, ","))
	}
	if len(q.Classes) > 0 {
		classes := make([]string, len(q.Classes))
		for i, c := range q.Classes {
			classes[i] = quote(c)
		}
		add("class=" + strings.Join(classes, ","))
	}
	if q.Floor != "" {
		add("on", quote(string(q.Floor)))
	}
	if q.Origin != nil {
		add("to", q.Origin.text())
	}
	if q.WithinM > 0 {
		add("within", num(q.WithinM)+"m")
	}
	if q.Visible {
		add("visible")
	}
	if q.PathWithinM > 0 {
		add("path", num(q.PathWithinM)+"m")
	}
	if b := q.Box; b != nil {
		add("in", "box", num(b.Min.X)+","+num(b.Min.Z)+","+num(b.Max.X)+","+num(b.Max.Z))
	}
	if q.Region != "" {
		add("in", "region", quote(q.Region))
	}
	if len(q.Polygon) > 0 {
		add("in", "polygon")
		for _, v := range q.Polygon {
			add(num(v.X) + "," + num(v.Z))
		}
	}
	for _, r := range q.Relations {
		add(strings.ReplaceAll(string(r.Kind), "_", "-"), r.To.text())
		if r.Kind == RelNear {
			add(num(r.DistanceM) + "m")
		}
	}
	if !q.SeenSince.IsZero() {
		add("since", instantText(q.SeenSince))
	}
	if !q.SeenBefore.IsZero() {
		add("before", instantText(q.SeenBefore))
	}
	if !q.ValidAt.IsZero() {
		add("at", instantText(q.ValidAt))
	}
	if q.Order != OrderDefault {
		add("order", string(q.Order))
		if q.Descending {
			add("desc")
		}
	}
	if q.Limit > 0 {
		add("limit", strconv.Itoa(q.Limit))
	}
	return strings.Join(parts, " ")
}
