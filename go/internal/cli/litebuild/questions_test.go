package litebuild

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// scripted answers the questions from a map attribute name -> answer key
// (the NAME=VALUE text). A Text answer can list several keys separated by
// "|": the first one that validate accepts is the answer.
type scripted struct {
	t        *testing.T
	answers  map[string]string
	asked    []string            // attributes with a question, in order
	options  map[string][]string // option keys of each question
	infos    map[string]string   // attribute -> reason of Info
	rejected map[string][]string // Text answers that validate refused
	defaults map[string]any
}

func newScripted(t *testing.T, answers map[string]string) *scripted {
	return &scripted{
		t: t, answers: answers,
		options:  map[string][]string{},
		infos:    map[string]string{},
		rejected: map[string][]string{},
		defaults: map[string]any{},
	}
}

func (s *scripted) answer(q Question) string {
	s.t.Helper()
	s.asked = append(s.asked, q.Attr.Name)
	s.defaults[q.Attr.Name] = q.Default
	keys := make([]string, len(q.Options))
	for i, v := range q.Options {
		keys[i] = q.Attr.Key(v)
	}
	s.options[q.Attr.Name] = keys
	key, ok := s.answers[q.Attr.Name]
	if !ok {
		s.t.Fatalf("unexpected question %s (options %v)", q.Attr.Name, keys)
	}
	return key
}

func (s *scripted) Select(q Question) (int, error) {
	key := s.answer(q)
	for i, v := range q.Options {
		if q.Attr.Key(v) == key {
			return i, nil
		}
	}
	s.t.Fatalf("%s: answer %s is not an option of %v", q.Attr.Name, key, s.options[q.Attr.Name])
	return 0, nil
}

func (s *scripted) Confirm(q Question) (bool, error) {
	key := s.answer(q)
	b, err := strconv.ParseBool(key)
	if err != nil {
		s.t.Fatalf("%s: %v", q.Attr.Name, err)
	}
	return b, nil
}

func (s *scripted) Text(q Question, validate func(string) error) (string, error) {
	for _, key := range strings.Split(s.answer(q), "|") {
		if validate(key) == nil {
			return key, nil
		}
		s.rejected[q.Attr.Name] = append(s.rejected[q.Attr.Name], key)
	}
	s.t.Fatalf("%s: no accepted answer", q.Attr.Name)
	return "", nil
}

func (s *scripted) Info(attr *Attribute, _ any, reason string) { s.infos[attr.Name] = reason }

func newTestConfigurator(t *testing.T, conf string) *Configurator {
	t.Helper()
	m, err := Load([]byte(conf), "test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c, err := NewConfigurator(m, nil)
	if err != nil {
		t.Fatalf("configurator: %v", err)
	}
	return c
}

func orderNames(c *Configurator) []string {
	var names []string
	for _, a := range c.Order() {
		names = append(names, a.Name)
	}
	return names
}

// TestRunCustomBoard checks a custom board: the user gives the hardware,
// a rule shows the value it sets, and the internal attributes get their
// values without a question.
func TestRunCustomBoard(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  board: {type: string, enum: [custom, xiao]}
  chip: {type: string, enum: [esp32c6, esp32s3]}
  idf_version: {type: string, enum: ["6.0.3", "6.1"]}
  idf_preview: {type: boolean}
  flash_size: {type: size, enum: [4MiB, 8MiB]}
  spram_size: {type: size}
  app_type: {type: string, enum: [native, wasm]}
  wendy_net: {type: boolean}
display:
  board: {title: Board, priority: 1}
  chip: {title: Chip, priority: 2}
  flash_size: {title: Flash size, priority: 3}
  app_type: {title: Application type, priority: 4}
  wendy_net: {title: Wendy Net, priority: 5}
rules:
  - if: board == "custom"
    set: {spram_size: 0, wendy_net: false}
  - if: board == "xiao"
    set: {chip: esp32s3, flash_size: 8MiB, spram_size: 8MiB}
  - if: chip == "esp32c6" || chip == "esp32s3"
    set: {idf_version: "6.0.3", idf_preview: false}
  - if: flash_size == 8MiB
    sdkconfig: CONFIG_FLASH_8MB=y
  - if: spram_size > 0
    sdkconfig: CONFIG_SPIRAM=y
  - if: app_type == "wasm"
    sdkconfig: CONFIG_WASM=y
  - if: wendy_net
    sdkconfig: CONFIG_WENDY_NET=y
`)
	p := newScripted(t, map[string]string{"board": "custom", "chip": "esp32c6", "flash_size": "8MiB", "app_type": "wasm"})
	res, err := c.Run(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"board", "chip", "flash_size", "app_type"}; !slices.Equal(p.asked, want) {
		t.Errorf("asked %v, want %v", p.asked, want)
	}
	// spram_size, idf_version and idf_preview are internal: not shown.
	want := map[string]string{"wendy_net": "set by Board"}
	if len(p.infos) != len(want) || p.infos["wendy_net"] != want["wendy_net"] {
		t.Errorf("infos = %v, want %v", p.infos, want)
	}
	if v := res.Solution.Values; v["spram_size"] != int64(0) || v["idf_version"] != "6.0.3" || v["idf_preview"] != false {
		t.Errorf("internal values = %v", v)
	}
	if res.Solution.Values["wendy_net"] != false {
		t.Errorf("wendy_net = %v, want false", res.Solution.Values["wendy_net"])
	}
	if got, want := res.Solution.Sdkconfig, []string{"CONFIG_FLASH_8MB=y", "CONFIG_WASM=y"}; !slices.Equal(got, want) {
		t.Errorf("sdkconfig = %v, want %v", got, want)
	}
}

func TestForbiddenValues(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  board: {type: string, enum: [a, b, c]}
  mode: {type: string, enum: [x, y]}
display:
  board: {default: c}
  mode:
rules:
  - if: board == "a" && mode == "y"
    valid: false
  - if: board == "c"
    valid: false
  - if: board == "a"
    sdkconfig: CONFIG_A=y
  - if: mode == "x"
    sdkconfig: CONFIG_X=y
`)
	p := newScripted(t, map[string]string{"board": "a"})
	res, err := c.Run(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.options["board"]; !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("board options = %v, want [a b]", got)
	}
	if p.defaults["board"] != nil {
		t.Errorf("board default = %v, want none (c is not allowed)", p.defaults["board"])
	}
	if p.infos["mode"] != "only allowed value" || res.Inputs["mode"] != "x" {
		t.Errorf("mode: info %q, input %v, want the only allowed value x", p.infos["mode"], res.Inputs["mode"])
	}
	if want := []string{"CONFIG_A=y", "CONFIG_X=y"}; !slices.Equal(res.Solution.Sdkconfig, want) {
		t.Errorf("sdkconfig = %v, want %v", res.Solution.Sdkconfig, want)
	}
}

// TestConflictLookahead checks that an answer is offered only when a later
// answer avoids a conflict, and that a rule with "!net" is not applied
// before the net question.
func TestConflictLookahead(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  board: {type: string, enum: [a, b]}
  net: {type: boolean}
  ram: {type: size, enum: [0, 8MiB]}
  mode: {type: string, enum: [m1, m2]}
display:
  board:
  net:
  mode:
rules:
  - if: board == "a"
    set: {ram: 0}
  - if: net
    set: {ram: 8MiB}
  - if: board == "b" && !net
    set: {mode: m1}
  - if: board == "b"
    set: {mode: m2}
  - if: ram == 8MiB || ram == 0
    sdkconfig: CONFIG_RAM=y
  - if: mode == "m1" || mode == "m2"
    sdkconfig: CONFIG_MODE=y
`)
	// board=a: net=true gives a ram conflict, so net=false is the only value.
	pa := newScripted(t, map[string]string{"board": "a", "mode": "m1"})
	if _, err := c.Run(pa); err != nil {
		t.Fatal(err)
	}
	if pa.infos["net"] != "only allowed value" {
		t.Errorf("board=a: net info = %q, want only allowed value", pa.infos["net"])
	}
	// board=b: net=false gives a mode conflict, so net=true is the only
	// value. Before the net question, "!net" must not count as true.
	pb := newScripted(t, map[string]string{"board": "b"})
	res, err := c.Run(pb)
	if err != nil {
		t.Fatal(err)
	}
	if got := pa.options["board"]; !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("board options = %v, want [a b]", got)
	}
	if pb.infos["net"] != "only allowed value" || res.Inputs["net"] != true {
		t.Errorf("board=b: net info %q, input %v, want the only allowed value true", pb.infos["net"], res.Inputs["net"])
	}
	if res.Solution.Values["ram"] != int64(8<<20) || res.Solution.Values["mode"] != "m2" {
		t.Errorf("values = %v", res.Solution.Values)
	}
}

func TestQuestionOrderPriority(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  x: {type: string, enum: ["1", "2"]}
  y: {type: boolean}
  z: {type: boolean}
display:
  x:
  y: {priority: 2}
  z: {priority: 1}
rules:
  - if: x == "1"
    set: {z: true}
  - if: y || z
    sdkconfig: CONFIG_YZ=y
`)
	// z comes first and pulls x, its dependency, before it.
	if got, want := orderNames(c), []string{"x", "z", "y"}; !slices.Equal(got, want) {
		t.Errorf("question order = %v, want %v", got, want)
	}
	p := newScripted(t, map[string]string{"x": "2", "z": "false", "y": "true"})
	if _, err := c.Run(p); err != nil {
		t.Fatal(err)
	}
	if want := []string{"x", "z", "y"}; !slices.Equal(p.asked, want) {
		t.Errorf("asked %v, want %v", p.asked, want)
	}
}

func TestTextQuestion(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  board: {type: string, enum: [small, big]}
  psram: {type: size}
display:
  board:
  psram:
rules:
  - if: psram > 0 && board == "small"
    valid: false
  - if: psram >= 8MiB
    sdkconfig: CONFIG_BIG_PSRAM=y
  - if: board == "small"
    sdkconfig: CONFIG_SMALL=y
`)
	p := newScripted(t, map[string]string{"board": "small", "psram": "8MiB|abc|0"})
	res, err := c.Run(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.rejected["psram"]; !slices.Equal(got, []string{"8MiB", "abc"}) {
		t.Errorf("rejected %v, want [8MiB abc]", got)
	}
	if res.Inputs["psram"] != int64(0) {
		t.Errorf("psram = %v, want 0", res.Inputs["psram"])
	}
}

// TestInternalAttributes checks that an attribute without a display entry is
// never shown: a value that a rule sets and an only allowed value give no
// Info, and a question on it is an error.
func TestInternalAttributes(t *testing.T) {
	const base = `
attributes:
  board: {type: string, enum: [a, b]}
  idf: {type: string, enum: ["5", "6"]}
  mode: {type: string, enum: [x, y]}
  extra: {type: size}
display:
  board: {title: Board}
rules:
  - if: board == "a"
    set: {idf: "6"}
  - else:
    set: {idf: "5"}
  - if: idf == "6"
    sdkconfig: CONFIG_IDF6=y
  - if: mode == "x"
    sdkconfig: CONFIG_X=y
`
	t.Run("hidden values", func(t *testing.T) {
		c := newTestConfigurator(t, base+"  - if: mode == \"y\"\n    valid: false\n")
		p := newScripted(t, map[string]string{"board": "a"})
		res, err := c.Run(p)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.asked, []string{"board"}) || len(p.infos) != 0 {
			t.Errorf("asked %v, infos %v: want only the board question and no info", p.asked, p.infos)
		}
		// idf comes from a rule; mode is the only allowed value, so it is an
		// input (build profile) although it is not shown.
		if res.Solution.Values["idf"] != "6" || res.Inputs["idf"] != nil || res.Inputs["mode"] != "x" {
			t.Errorf("inputs %v, values %v", res.Inputs, res.Solution.Values)
		}
		if _, ok := res.Inputs["extra"]; ok {
			t.Errorf("extra is not needed, it must stay undefined")
		}
	})
	for _, tc := range []struct{ name, rules, attr string }{
		// mode has two allowed values.
		{"choice", "", "mode"},
		// mode has one allowed value; extra has no enum and is needed.
		{"text", "  - if: mode == \"y\"\n    valid: false\n  - if: extra > 0\n    sdkconfig: CONFIG_EXTRA=y\n", "extra"},
	} {
		t.Run("question on "+tc.name, func(t *testing.T) {
			c := newTestConfigurator(t, base+tc.rules)
			_, err := c.Run(newScripted(t, map[string]string{"board": "a"}))
			want := `test: attribute "` + tc.attr + `" needs a question, but it is internal (add it to the display section)`
			if err == nil || err.Error() != want {
				t.Fatalf("Run error = %v, want %s", err, want)
			}
		})
	}
}

func TestNoConfiguration(t *testing.T) {
	c := newTestConfigurator(t, `
attributes:
  board: {type: string, enum: [a, b]}
rules:
  - if: board == "a"
    sdkconfig: CONFIG_A=y
  - if: board == "a" || board == "b"
    valid: false
`)
	_, err := c.Run(newScripted(t, nil))
	if !errors.Is(err, ErrNoConfiguration) {
		t.Fatalf("Run error = %v, want ErrNoConfiguration", err)
	}
}
