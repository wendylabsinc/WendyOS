package litebuild

import (
	"strings"
	"testing"
)

func TestBuildProfile(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  board: {type: string}
  flash_size: {type: size}
  spram_size: {type: size}
  wendy_net: {type: boolean}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]any{
		"board":      "yes",
		"flash_size": int64(8 << 20),
		"spram_size": int64(0),
		"wendy_net":  true,
	}
	wantProfile := "board: 'yes'\nflash_size: 8MiB\nspram_size: 0\nwendy_net: true"
	if got := m.BuildProfile(inputs); got != wantProfile {
		t.Errorf("BuildProfile = %q, want %q", got, wantProfile)
	}
	if got := m.BuildProfile(nil); got != "" {
		t.Errorf("BuildProfile(nil) = %q, want empty", got)
	}
	wantLine := `CONFIG_WENDY_BUILD_PROFILE="a: \"x\"\\nb: c\\d"`
	if got := BuildProfileLine("a: \"x\"\nb: c\\d"); got != wantLine {
		t.Errorf("BuildProfileLine = %s, want %s", got, wantLine)
	}
}

// TestYAMLString checks the output of yaml.safe_dump({"k": s}) with PyYAML
// 6.0.3 for each string.
func TestYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		"seeed_xiao": "seeed_xiao",
		"esp32c6":    "esp32c6",
		"4MiB":       "4MiB",
		"yes":        "'yes'",
		"Off":        "'Off'",
		"123":        "'123'",
		"0x1F":       "'0x1F'",
		"1.5":        "'1.5'",
		"null":       "'null'",
		"":           "''",
		"a b":        "a b",
		"2024-01-02": "'2024-01-02'",
		"it's":       "it's",
		"a/b":        "a/b",
		"-x":         "-x",
		"a.b-c":      "a.b-c",
	} {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDependencyCycle(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  a: {type: boolean}
  b: {type: boolean}
rules:
  - if: a
    set: {b: true}
  - if: b
    set: {a: true}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSolver(m)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle: a -> b -> a") {
		t.Fatalf("NewSolver error = %v, want a dependency cycle", err)
	}
}

func TestConflict(t *testing.T) {
	m, err := Load([]byte(`
attributes:
  board: {type: string, enum: [a]}
  chip: {type: string}
rules:
  - if: board == "a"
    set: {chip: x}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	solver, err := NewSolver(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = solver.Evaluate(map[string]any{"board": "a", "chip": "y"}, map[string]string{"chip": "command line"})
	want := `conflict on chip: rules[0] (if board == "a") sets x, but command line set y`
	if err == nil || err.Error() != want {
		t.Fatalf("Evaluate error = %v, want %s", err, want)
	}
}
