package arduinotoolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func stubExec(t *testing.T, fake func(ctx context.Context, name string, args ...string) *exec.Cmd) *[][]string {
	t.Helper()
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	var calls [][]string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return fake(ctx, name, args...)
	}
	return &calls
}

func writeSketchYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "sketch.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const pinnedProfile = `profiles:
  default:
    fqbn: arduino:zephyr:unoq
    platforms:
      - platform: arduino:zephyr (1.1.0)
default_profile: default
`

func TestLoadProfile(t *testing.T) {
	tests := []struct {
		name, yaml, profile, want, wantErr string
	}{
		{name: "missing file", wantErr: "arduino:zephyr (1.1.0)"},
		{name: "default profile", yaml: pinnedProfile, want: "default"},
		{name: "named profile", yaml: pinnedProfile, profile: "default", want: "default"},
		{name: "unknown profile", yaml: pinnedProfile, profile: "other", wantErr: `"other"`},
		{name: "unpinned platform", yaml: strings.Replace(pinnedProfile, "arduino:zephyr (1.1.0)", "arduino:zephyr", 1), wantErr: "pin"},
		{name: "other board", yaml: strings.Replace(pinnedProfile, "arduino:zephyr:unoq", "arduino:avr:uno", 1), wantErr: "arduino:zephyr:unoq"},
		{name: "no zephyr platform", yaml: strings.Replace(pinnedProfile, "arduino:zephyr (1.1.0)", "arduino:mbed (4.1.1)", 1), wantErr: "does not pin arduino:zephyr"},
		{name: "single profile without default", yaml: strings.Replace(pinnedProfile, "default_profile: default\n", "", 1), want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, core, err := LoadProfile(writeSketchYAML(t, tt.yaml), tt.profile)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadProfile() = %q, %v; want error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want || core != "1.1.0" {
				t.Fatalf("LoadProfile() = %q, %q, %v; want %q, 1.1.0", got, core, err, tt.want)
			}
		})
	}
}

func TestMissingSketchYAMLSnippetListsRouterBridge(t *testing.T) {
	_, _, err := LoadProfile(writeSketchYAML(t, ""), "")
	if err == nil || !strings.Contains(err.Error(), "Arduino_RouterBridge (0.4.4)") {
		t.Fatalf("error = %v, want a snippet with Arduino_RouterBridge (0.4.4)", err)
	}
}

func TestLayoutForCore(t *testing.T) {
	for _, v := range []string{"1.0.0", "1.1.3"} {
		if got, err := LayoutForCore(v); err != nil || got != "zephyr-1" {
			t.Errorf("LayoutForCore(%q) = %q, %v; want zephyr-1", v, got, err)
		}
	}
	for _, v := range []string{"0.53.1", "2.0.0", "1.10.0", "", "1"} {
		_, err := LayoutForCore(v)
		want := "unsupported Arduino core version " + v + "; use 1.0 or 1.1"
		if err == nil || err.Error() != want {
			t.Errorf("LayoutForCore(%q) error = %v; want %q", v, err, want)
		}
	}
}

func TestCompileReadsArtifacts(t *testing.T) {
	withCacheDir(t, t.TempDir())
	props, err := filepath.Abs("testdata/properties.txt")
	if err != nil {
		t.Fatal(err)
	}
	calls := stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if slices.Contains(args, "--show-properties=expanded") {
			return exec.CommandContext(ctx, "cat", props)
		}
		return exec.CommandContext(ctx, "true")
	})
	got, err := Compile(context.Background(), "/bin/arduino-cli", "/proj/sketch", "default", "/build", nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := Artifacts{
		LoaderPath:  "/cache/arduino/data/internal/arduino_zephyr_1.1.0_9d917e296ff4622f/firmwares/zephyr-arduino_uno_q_stm32u585xx.elf",
		SketchPath:  "/build/sketch.ino.elf-zsk.bin",
		CoreVersion: "1.1.0",
		Layout:      "zephyr-1",
	}
	if *got != want {
		t.Fatalf("Compile = %+v, want %+v", *got, want)
	}
	if len(*calls) != 2 {
		t.Fatalf("arduino-cli calls = %v, want compile then show-properties", *calls)
	}
	first := strings.Join((*calls)[0], " ")
	for _, part := range []string{"--config-file", "compile", "--profile default", "--build-path /build", "/proj/sketch"} {
		if !strings.Contains(first, part) {
			t.Errorf("compile call %q lacks %q", first, part)
		}
	}
}

func TestCompileFailureStops(t *testing.T) {
	withCacheDir(t, t.TempDir())
	calls := stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	})
	if _, err := Compile(context.Background(), "/bin/arduino-cli", "/proj/sketch", "default", "/build", nil, nil); err == nil {
		t.Fatal("Compile succeeded after arduino-cli failed")
	}
	if len(*calls) != 1 {
		t.Fatalf("calls after a failed compile = %d, want 1", len(*calls))
	}
}

func TestCompileFailureShowsTheCompilerErrors(t *testing.T) {
	withCacheDir(t, t.TempDir())
	out := "Used library A\n/proj/sketch/sketch.ino:38:37: error: expected ';' before 'x'\n   38 |   foo()\n/proj/sketch/a.h:2:10: fatal error: missing.h: No such file or directory\nUsed platform  Version\n"
	stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$0\"; exit 1", out)
	})
	var shown strings.Builder
	_, err := Compile(context.Background(), "/bin/arduino-cli", "/proj/sketch", "default", "/build", &shown, &shown)
	if err == nil || !strings.Contains(err.Error(), "sketch.ino:38:37: error: expected ';'") || !strings.Contains(err.Error(), "fatal error: missing.h") {
		t.Fatalf("err = %v, want both compiler error lines", err)
	}
	if strings.Contains(err.Error(), "Used library") || !strings.Contains(shown.String(), "Used library A") {
		t.Fatalf("err = %v, shown = %q; want only error lines in the error and the full output still shown", err, shown.String())
	}
}

func TestCheckHostAppleSiliconWithoutRosetta(t *testing.T) {
	withHost(t, "darwin/arm64")
	calls := stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	})
	err := CheckHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "softwareupdate --install-rosetta") {
		t.Fatalf("CheckHost() = %v, want the Rosetta install hint", err)
	}
	if len(*calls) != 1 || (*calls)[0][0] != "arch" {
		t.Fatalf("calls = %v, want one arch probe", *calls)
	}
}

func TestCheckHostAppleSiliconWithRosetta(t *testing.T) {
	withHost(t, "darwin/arm64")
	stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})
	if err := CheckHost(context.Background()); err != nil {
		t.Fatalf("CheckHost() = %v, want nil", err)
	}
}

func TestCheckHostOtherHostsSkipProbe(t *testing.T) {
	for _, host := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "windows/amd64"} {
		withHost(t, host)
		calls := stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "false")
		})
		if err := CheckHost(context.Background()); err != nil || len(*calls) != 0 {
			t.Fatalf("%s: CheckHost() = %v with %d probes, want nil and none", host, err, len(*calls))
		}
	}
}

// The compiler writes stdout and stderr at once; run with -race.
func TestCompileLogsBothStreamsSafely(t *testing.T) {
	withCacheDir(t, t.TempDir())
	stubExec(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "for i in $(seq 200); do echo out; echo err >&2; done; echo 'a.ino:1:1: error: boom' >&2; exit 1")
	})
	var shown strings.Builder
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return shown.Write(p)
	})
	if _, err := Compile(context.Background(), "/bin/arduino-cli", "/proj/sketch", "default", "/build", w, w); err == nil || !strings.Contains(err.Error(), "a.ino:1:1: error: boom") {
		t.Fatalf("err = %v, want the compiler error", err)
	}
	if n := strings.Count(shown.String(), "out\n") + strings.Count(shown.String(), "err\n"); n != 400 {
		t.Fatalf("shown %d of 400 lines", n)
	}
}
