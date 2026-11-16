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
	wantProfile := `{"board":"yes","flash_size":"8MiB","spram_size":0,"wendy_net":true}`
	if got := m.BuildProfile(inputs); got != wantProfile {
		t.Errorf("BuildProfile = %q, want %q", got, wantProfile)
	}
	if got := m.BuildProfile(nil); got != "" {
		t.Errorf("BuildProfile(nil) = %q, want empty", got)
	}
	wantLine := `CONFIG_WENDY_BUILD_PROFILE="{\"a\":\"x\\\\y\"}"`
	if got := BuildProfileLine(`{"a":"x\\y"}`); got != wantLine {
		t.Errorf("BuildProfileLine = %s, want %s", got, wantLine)
	}
}

// TestJSONString checks the output of json.dumps(s, ensure_ascii=False)
// with Python 3 for each string.
func TestJSONString(t *testing.T) {
	for in, want := range map[string]string{
		"seeed_xiao": `"seeed_xiao"`,
		"4MiB":       `"4MiB"`,
		"":           `""`,
		`a"b\c`:      `"a\"b\\c"`,
		"<&>":        `"<&>"`,
	} {
		if got := jsonString(in); got != want {
			t.Errorf("jsonString(%q) = %s, want %s", in, got, want)
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
