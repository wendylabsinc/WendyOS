package litebuild

import (
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
		{"format version", `format_version: "2.0"` + attrs, `test: format_version '2.0' is not supported (supported: "1.0")`},
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
