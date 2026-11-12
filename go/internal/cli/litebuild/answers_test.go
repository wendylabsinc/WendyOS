package litebuild

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// previousConf has a question of each kind whose allowed answers depend on
// the board.
const previousConf = `
attributes:
  board: {type: string, enum: [a, b]}
  mode: {type: string, enum: [x, y, z]}
  psram: {type: size}
  net: {type: boolean}
display:
  board: {default: a}
  mode: {default: x}
  psram: {default: 0}
  net:
rules:
  - if: board == "a" && mode == "z"
    valid: false
  - if: psram > 0 && board == "a"
    valid: false
  - if: board == "a" || board == "b"
    sdkconfig: CONFIG_BOARD=y
  - if: mode == "x" || mode == "y" || mode == "z"
    sdkconfig: CONFIG_MODE=y
  - if: psram >= 0
    sdkconfig: CONFIG_PSRAM=y
  - if: net
    sdkconfig: CONFIG_NET=y
  - if: "!net"
    sdkconfig: CONFIG_NO_NET=y
`

// TestPreviousDefaults checks that a previous answer is the default of its
// question when it is allowed, and that display.default is used otherwise.
func TestPreviousDefaults(t *testing.T) {
	c := newTestConfigurator(t, previousConf)

	c.Previous = map[string]any{"board": "b", "mode": "y", "psram": int64(4 << 20), "net": true}
	p := newScripted(t, map[string]string{"board": "b", "mode": "y", "psram": "4MiB", "net": "true"})
	if _, err := c.Run(p); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"board": "b", "mode": "y", "psram": int64(4 << 20), "net": true}
	if !maps.Equal(p.defaults, want) {
		t.Errorf("defaults = %v, want the previous answers %v", p.defaults, want)
	}

	// board=a forbids mode z and a psram above 0: those previous answers are
	// not allowed, so display.default is the default.
	c.Previous = map[string]any{"board": "b", "mode": "z", "psram": int64(4 << 20), "net": false}
	p = newScripted(t, map[string]string{"board": "a", "mode": "x", "psram": "0", "net": "false"})
	if _, err := c.Run(p); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"board": "b", "mode": "x", "psram": int64(0), "net": false}
	if !maps.Equal(p.defaults, want) {
		t.Errorf("defaults = %v, want %v", p.defaults, want)
	}
}

func TestSaveLoadAnswers(t *testing.T) {
	m, err := Load([]byte(previousConf), "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache", "answers.json")

	if got := LoadAnswers(m, path); len(got) != 0 {
		t.Errorf("LoadAnswers without a file = %v, want no values", got)
	}

	previous := map[string]any{"board": "a", "mode": "x", "psram": int64(8 << 20)}
	inputs := map[string]any{"board": "b", "mode": "y", "net": true, "unknown": "v"}
	if err := SaveAnswers(m, path, previous, inputs); err != nil {
		t.Fatal(err)
	}
	// The inputs win; psram, not asked in this run, is kept.
	want := map[string]any{"board": "b", "mode": "y", "psram": int64(8 << 20), "net": true}
	if got := LoadAnswers(m, path); !maps.Equal(got, want) {
		t.Errorf("LoadAnswers = %v, want %v", got, want)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("cache directory has %d files, want only answers.json", len(entries))
	}
}

// TestLoadAnswersSkipsStaleValues checks that values from an older master
// configuration are dropped, not reported.
func TestLoadAnswersSkipsStaleValues(t *testing.T) {
	m, err := Load([]byte(previousConf), "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "answers.json")
	if err := os.WriteFile(path, []byte(`{"board": "gone", "mode": "y", "removed": "1", "psram": "abc", "net": "maybe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := LoadAnswers(m, path), map[string]any{"mode": "y"}; !maps.Equal(got, want) {
		t.Errorf("LoadAnswers = %v, want %v", got, want)
	}

	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadAnswers(m, path); len(got) != 0 {
		t.Errorf("LoadAnswers of a corrupt file = %v, want no values", got)
	}
}
