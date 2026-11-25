package litebuild

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadErrors(t *testing.T) {
	const attrs = `
attributes:
  board: {type: string, enum: [a, b]}
  net: {type: boolean}
`
	tests := []struct {
		name, conf, want string
	}{
		{"not a map", `[1]`, "test: expected a map with attributes and rules"},
		{"unknown key", attrs + "extra: 1\n", "test: unknown key(s): extra"},
		{"format version", `format_version: "2.0"` + attrs, `test: format_version '2.0' is not supported (supported: 1.x)`},
		{"format version before keys", `format_version: "2.0"` + attrs + "extra: 1\n", `test: format_version '2.0' is not supported (supported: 1.x)`},
		{"no attributes", "rules: []\n", "test: attributes is missing"},
		{"duplicate key", attrs + "  net: {type: boolean}\n", "'net' is given twice"},
		{"bad type", "attributes:\n  a: {type: int}\n", "test: attributes: a: type must be one of: string, size, boolean"},
		{"boolean enum", "attributes:\n  a: {type: boolean, enum: [true]}\n", "test: attributes: a: a boolean has no enum"},
		{"reserved", "attributes:\n  sdkconfig: {type: boolean}\n", "test: attributes: sdkconfig: sdkconfig is intrinsic, it cannot be declared"},
		{"else first", attrs + "rules:\n  - else:\n    sdkconfig: X=y\n", "test: rules[0] (else): else must follow an if or else-if rule"},
		{"else condition", attrs + "rules:\n  - if: net\n    sdkconfig: X=y\n  - else: net\n    sdkconfig: Y=y\n", "else takes no condition"},
		{"valid true", attrs + "rules:\n  - if: net\n    valid: true\n", "valid must be false"},
		{"valid with set", attrs + "rules:\n  - if: net\n    valid: false\n    set: {board: a}\n", "valid: false cannot have set or sdkconfig"},
		{"sets nothing", attrs + "rules:\n  - if: net\n", "the rule sets nothing"},
		{"warn no condition", attrs + "rules:\n  - warn: {id: w1, message: M}\n", "test: rules[0]: warn needs an if, else-if or else"},
		{"warn with set", attrs + "rules:\n  - if: net\n    warn: {id: w1, message: M}\n    set: {board: a}\n", "warn cannot have set, sdkconfig or valid"},
		{"warn with valid", attrs + "rules:\n  - if: net\n    warn: {id: w1, message: M}\n    valid: false\n", "warn cannot have set, sdkconfig or valid"},
		{"warn not a map", attrs + "rules:\n  - if: net\n    warn: careful\n", "warn must be a map with id and message"},
		{"warn unknown key", attrs + "rules:\n  - if: net\n    warn: {id: w1, message: M, level: high}\n", "warn: unknown key(s): level"},
		{"warn no message", attrs + "rules:\n  - if: net\n    warn: {id: w1}\n", "warn: message must be a text"},
		{"warn empty id", attrs + "rules:\n  - if: net\n    warn: {id: \" \", message: M}\n", "warn: id must be a text"},
		{"bad enum value", attrs + "rules:\n  - if: board == \"c\"\n    sdkconfig: X=y\n", "board: c is not one of: a, b"},
		{"set unknown", attrs + "rules:\n  - set: {chip: x}\n", `set: unknown attribute "chip"`},
		{"display unknown attribute", attrs + "display:\n  chip: {title: Chip}\n", "test: display: chip: unknown attribute"},
		{"display unknown key", attrs + "display:\n  board: {label: Board}\n", "test: display: board: unknown key(s): label"},
		{"display bad value", attrs + "display:\n  board:\n    values: {c: C}\n", "test: display: board: values: board: c is not one of: a, b"},
		{"display bad default", attrs + "display:\n  board: {default: c}\n", "default: board: c is not one of: a, b"},
		{"display bad priority", attrs + "display:\n  board: {priority: high}\n", "priority must be an integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.conf), "test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadChainAndDisplay(t *testing.T) {
	m, err := Load([]byte(`
format_version: "1.0"
attributes:
  board: {type: string, enum: [a, b, c]}
  net: {type: boolean}
  flash: {type: size, enum: [4MiB, 8MiB]}
rules:
  - if: board == "a"
    sdkconfig: |
      # comment line is kept
      CONFIG_A=y

  - else-if: board == "b"
    sdkconfig: [CONFIG_B=y]
  - else:
    set: {net: false}
display:
  board:
    title: Board
    help: Select a board.
    priority: 3
    default: b
    values: {a: Board A}
  net:
    values: {true: Enabled, false: Disabled}
  flash:
    values: {8MiB: Eight}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	conds := []string{`board == "a"`, `!(board == "a") && board == "b"`, `!(board == "a") && !(board == "b")`}
	for i, want := range conds {
		if got := m.Rules[i].Condition.String(); got != want {
			t.Errorf("rules[%d] condition = %s, want %s", i, got, want)
		}
	}
	if got := m.Rules[0].Sdkconfig; len(got) != 2 || got[0] != "# comment line is kept" || got[1] != "CONFIG_A=y" {
		t.Errorf("rules[0] sdkconfig = %q", got)
	}
	if got := m.Rules[1].Name(); got != `rules[1] (else-if board == "b")` {
		t.Errorf("rules[1] name = %s", got)
	}
	board, net, flash := m.Attributes["board"], m.Attributes["net"], m.Attributes["flash"]
	if board.Title() != "Board" || board.Display.Help != "Select a board." || *board.Display.Priority != 3 || board.Display.Default != "b" {
		t.Errorf("board display = %+v", board.Display)
	}
	if board.Label("a") != "Board A" || board.Label("c") != "c" {
		t.Errorf("board labels: %s, %s", board.Label("a"), board.Label("c"))
	}
	if net.Title() != "net" || net.Label(true) != "Enabled" || net.Label(false) != "Disabled" {
		t.Errorf("net: %s, %s, %s", net.Title(), net.Label(true), net.Label(false))
	}
	if flash.Label(int64(8<<20)) != "Eight" || flash.Label(int64(4<<20)) != "4 MiB" {
		t.Errorf("flash labels: %s, %s", flash.Label(int64(8<<20)), flash.Label(int64(4<<20)))
	}
}

// TestVisible checks that an attribute with a display entry, even empty, is
// visible, and that the other attributes are internal.
func TestVisible(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  a: {type: boolean}
  b: {type: boolean}
  c: {type: boolean}
  d: {type: boolean}
display:
  a: {title: A}
  b: {}
  c:
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"a": true, "b": true, "c": true, "d": false} {
		if got := m.Attributes[name].Visible; got != want {
			t.Errorf("%s: Visible = %v, want %v", name, got, want)
		}
	}
}

func TestLoadWarning(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  net: {type: boolean}
rules:
  - if: net
    sdkconfig: CONFIG_NET=y
  - else:
    warn:
      id: " w1 "
      message: |
        Without Wendy Net, the device is alone.
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	rule := m.Rules[1]
	if rule.Warning == nil || *rule.Warning != (Warning{ID: "w1", Message: "Without Wendy Net, the device is alone."}) {
		t.Fatalf("warning = %+v", rule.Warning)
	}
	if len(rule.Targets()) != 0 || rule.Forbidden {
		t.Errorf("a warn rule has targets %v or is forbidden", rule.Targets())
	}
	s, err := NewSolver(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		net  bool
		want int
	}{{true, 0}, {false, 1}} {
		sol, err := s.Evaluate(map[string]any{"net": tc.net}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(sol.Warnings) != tc.want || !sol.Allowed() {
			t.Errorf("net=%v: warnings %d, allowed %v, want %d warnings, allowed", tc.net, len(sol.Warnings), sol.Allowed(), tc.want)
		}
		for _, r := range sol.Applied {
			if r.Warning != nil {
				t.Errorf("net=%v: warn rule in Applied", tc.net)
			}
		}
	}
}

func TestFormatVersion(t *testing.T) {
	const attrs = "\nattributes:\n  net: {type: boolean}\n"
	tests := []struct {
		line    string
		ok      bool
		version string
		newer   bool
	}{
		{"", true, "", false},
		{`format_version: "1.0"`, true, "1.0", false},
		{`format_version: "1.7"`, true, "1.7", false},
		{`format_version: 1.0`, true, "1.0", false},
		{`format_version: "1.12"`, true, "1.12", false},
		{`format_version: "2.0"`, false, "2.0", true},
		{`format_version: "10.3"`, false, "10.3", true},
		{`format_version: "0.9"`, false, "0.9", false},
		{`format_version: "1"`, false, "1", false},
		{`format_version: 1`, false, "1", false},
		{`format_version: "1.0.1"`, false, "1.0.1", false},
		{`format_version: abc`, false, "abc", false},
		{`format_version: true`, false, "True", false},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			data := []byte(tc.line + attrs)
			m, err := Load(data, "test")
			check := CheckFormatVersion(data)
			if tc.ok {
				if err != nil || check != nil {
					t.Fatalf("Load error %v, CheckFormatVersion %v, want none", err, check)
				}
				if m.FormatVersion != tc.version {
					t.Errorf("FormatVersion = %q, want %q", m.FormatVersion, tc.version)
				}
				return
			}
			for _, e := range []error{err, check} {
				var fe *FormatVersionError
				if !errors.As(e, &fe) {
					t.Fatalf("error %v, want a FormatVersionError", e)
				}
				if fe.Version != tc.version || fe.Newer != tc.newer {
					t.Errorf("FormatVersionError{Version: %q, Newer: %v}, want %q, %v", fe.Version, fe.Newer, tc.version, tc.newer)
				}
			}
		})
	}
	// Only format_version is read: the rest can be a newer format.
	if err := CheckFormatVersion([]byte("format_version: \"1.3\"\nnew_section: {x: 1}\n")); err != nil {
		t.Errorf("CheckFormatVersion = %v, want nil", err)
	}
	if err := CheckFormatVersion([]byte("[1]")); err != nil {
		t.Errorf("CheckFormatVersion(not a map) = %v, want nil", err)
	}
}
