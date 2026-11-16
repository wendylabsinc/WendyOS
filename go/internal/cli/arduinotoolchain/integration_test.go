package arduinotoolchain

import (
	"context"
	"os"
	"testing"
)

// Downloads the pinned arduino-cli and compiles a real sketch:
// WENDY_ARDUINO_SKETCH=<dir with sketch.yaml> go test -run Integration
func TestIntegrationCompileRealSketch(t *testing.T) {
	sketch := os.Getenv("WENDY_ARDUINO_SKETCH")
	if sketch == "" {
		t.Skip("set WENDY_ARDUINO_SKETCH to a sketch directory to run")
	}
	ctx := context.Background()
	cli, err := EnsureCLI(ctx, os.Stderr)
	if err != nil {
		t.Fatalf("EnsureCLI: %v", err)
	}
	profile, _, err := LoadProfile(sketch, "")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	a, err := Compile(ctx, cli, sketch, profile, t.TempDir(), os.Stderr, os.Stderr)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, p := range []string{a.LoaderPath, a.SketchPath} {
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			t.Errorf("artifact %s: %v", p, err)
		}
	}
	if a.Layout != "zephyr-1" {
		t.Errorf("layout = %q, want zephyr-1", a.Layout)
	}
}
