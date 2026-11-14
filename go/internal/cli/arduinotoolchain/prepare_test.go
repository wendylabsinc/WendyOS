package arduinotoolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What Arduino App Lab writes: no board, an unpinned core, no libraries.
const appLabYAML = `profiles:
  default:
    platforms:
      - platform: arduino:zephyr
default_profile: default
`

func writeSketch(t *testing.T, yaml, ino string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blink")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"sketch.yaml": yaml, "blink.ino": ino} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPrepareSketchFillsInAnAppLabProfile(t *testing.T) {
	withCacheDir(t, t.TempDir())
	src := writeSketch(t, appLabYAML, "#include <Arduino_RouterBridge.h>\nvoid setup(){}\nvoid loop(){}\n")
	p, err := PrepareSketch(src, "")
	if err != nil {
		t.Fatalf("PrepareSketch: %v", err)
	}
	if p.Dir == src || filepath.Base(p.Dir) != "blink" || p.Profile != "default" || p.Core != DefaultCore {
		t.Fatalf("prepared = %+v, want a staged copy named blink building %s", p, DefaultCore)
	}
	staged, err := os.ReadFile(filepath.Join(p.Dir, "sketch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fqbn: arduino:zephyr:unoq", "arduino:zephyr (1.1.0)", "Arduino_RouterBridge (0.4.4)", "dependency: MsgPack (0.4.2)"} {
		if !strings.Contains(string(staged), want) {
			t.Errorf("staged sketch.yaml lacks %q:\n%s", want, staged)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "blink.ino")); err != nil {
		t.Errorf("staged copy lacks the sketch: %v", err)
	}
	if len(p.Notices) != 3 {
		t.Errorf("notices = %q, want one each for the board, the core and the libraries", p.Notices)
	}
	if orig, _ := os.ReadFile(filepath.Join(src, "sketch.yaml")); string(orig) != appLabYAML {
		t.Error("the project's sketch.yaml was edited")
	}
}

func TestPrepareSketchLeavesACompleteProfileAlone(t *testing.T) {
	withCacheDir(t, t.TempDir())
	src := writeSketch(t, pinnedProfile, "void setup(){}\nvoid loop(){}\n")
	p, err := PrepareSketch(src, "")
	if err != nil || p.Dir != src || len(p.Notices) != 0 || p.Core != "1.1.0" {
		t.Fatalf("prepared = %+v, %v; want the project's own folder and no notices", p, err)
	}
}

func TestPrepareSketchAddsNoLibrariesWithoutTheBridge(t *testing.T) {
	withCacheDir(t, t.TempDir())
	p, err := PrepareSketch(writeSketch(t, appLabYAML, "void setup(){}\nvoid loop(){}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if staged, _ := os.ReadFile(filepath.Join(p.Dir, "sketch.yaml")); strings.Contains(string(staged), "libraries") {
		t.Errorf("libraries added to a sketch that does not use the Bridge:\n%s", staged)
	}
}

func TestPrepareSketchStillRefusesWhatItCannotDefault(t *testing.T) {
	withCacheDir(t, t.TempDir())
	if _, err := PrepareSketch(writeSketch(t, "", ""), ""); err == nil {
		t.Error("an empty sketch.yaml was accepted")
	}
	other := strings.Replace(pinnedProfile, "arduino:zephyr:unoq", "arduino:avr:uno", 1)
	if _, err := PrepareSketch(writeSketch(t, other, ""), ""); err == nil || !strings.Contains(err.Error(), "arduino:zephyr:unoq") {
		t.Errorf("another board: err = %v, want the board refusal", err)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := PrepareSketch(missing, ""); err == nil || !strings.Contains(err.Error(), "no sketch.yaml") {
		t.Errorf("missing sketch.yaml: err = %v, want the example", err)
	}
}

func TestPrepareSketchAddsOnlyTheMissingBridgeLibraries(t *testing.T) {
	withCacheDir(t, t.TempDir())
	yaml := strings.Replace(pinnedProfile, "default_profile:", "    libraries:\n      - Arduino_Modulino (0.10.0)\n      - dependency: MsgPack (0.4.1)\ndefault_profile:", 1)
	p, err := PrepareSketch(writeSketch(t, yaml, "#include <Arduino_RouterBridge.h>\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	staged, _ := os.ReadFile(filepath.Join(p.Dir, "sketch.yaml"))
	text := string(staged)
	for _, want := range []string{"Arduino_Modulino (0.10.0)", "Arduino_RouterBridge (0.4.4)", "dependency: Arduino_RPClite (0.3.1)", "dependency: MsgPack (0.4.1)"} {
		if !strings.Contains(text, want) {
			t.Errorf("staged sketch.yaml lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "MsgPack (0.4.2)") {
		t.Errorf("a library the profile already pins was added again:\n%s", text)
	}
	if len(p.Notices) != 1 {
		t.Errorf("notices = %q, want one", p.Notices)
	}
}

func TestStageSketchKeepsUnchangedFilesAndDropsDeletedOnes(t *testing.T) {
	withCacheDir(t, t.TempDir())
	src := writeSketch(t, appLabYAML, "void setup(){}\nvoid loop(){}\n")
	extra := filepath.Join(src, "old.h")
	if err := os.WriteFile(extra, []byte("// old"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := PrepareSketch(src, "")
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(first.Dir, "blink.ino")
	before, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSketch(src, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(staged)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged file was rewritten: the next compile would not be incremental")
	}
	if _, err := os.Stat(filepath.Join(first.Dir, "old.h")); !os.IsNotExist(err) {
		t.Fatalf("a file deleted from the project is still in the copy: %v", err)
	}
}

func TestStageSketchSurvivesAFileBecomingAFolder(t *testing.T) {
	withCacheDir(t, t.TempDir())
	src := writeSketch(t, appLabYAML, "void setup(){}\nvoid loop(){}\n")
	thing := filepath.Join(src, "thing")
	if err := os.WriteFile(thing, []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSketch(src, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(thing); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(thing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(thing, "inner.h"), []byte("// inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareSketch(src, "")
	if err != nil {
		t.Fatalf("a file that became a folder broke the copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "thing", "inner.h")); err != nil {
		t.Fatalf("the new folder's file is missing from the copy: %v", err)
	}
}
