//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/litebuild"
)

func TestParseBuildAttrs(t *testing.T) {
	attrs, err := parseBuildAttrs([]string{"board=esp32c61_devkit_c_1", " cloud_tunnel = false "})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"board": "esp32c61_devkit_c_1", "cloud_tunnel": "false"}; !maps.Equal(attrs, want) {
		t.Errorf("attrs = %v, want %v", attrs, want)
	}
	for _, items := range [][]string{{"board"}, {"=x"}, {"board="}, {"board=a", "board=b"}} {
		if _, err := parseBuildAttrs(items); err == nil || !IsUsageError(err) {
			t.Errorf("%q: %v; want a usage error", items, err)
		}
	}
}

// liteBuildCall records a runLiteBuildFn call.
type liteBuildCall struct {
	version string
	attrs   map[string]string
	wifi    wifiCLIOptions
}

// stubLiteBuild replaces the Wendy Lite build, and fails the test on any
// manifest request.
func stubLiteBuild(t *testing.T) *[]liteBuildCall {
	t.Helper()
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()
	serveManifests(t, nil)
	var calls []liteBuildCall
	prev := runLiteBuildFn
	t.Cleanup(func() { runLiteBuildFn = prev })
	runLiteBuildFn = func(_ context.Context, _ bool, version string, attrs map[string]string, wifi wifiCLIOptions, _ string, _ preEnrollOptions) error {
		calls = append(calls, liteBuildCall{version, attrs, wifi})
		return nil
	}
	return &calls
}

func runInstallCmd(args ...string) error {
	cmd := newOSInstallCmd()
	// root's --json, which checkLiteBuildFlags reads.
	cmd.PersistentFlags().BoolVar(&jsonOutput, "json", jsonOutput, "")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// --device-type esp32 runs the Wendy Lite build without a terminal and
// without any manifest.
func TestInstallDeviceTypeESP32(t *testing.T) {
	calls := stubLiteBuild(t)
	err := runInstallCmd("--device-type", "esp32", "--no-wifi", "--version", "gab/configurator",
		"--build-attr", "board=esp32c61_devkit_c_1", "--build-attr", "cloud_tunnel=false")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("Wendy Lite build runs: %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if want := map[string]string{"board": "esp32c61_devkit_c_1", "cloud_tunnel": "false"}; !maps.Equal(call.attrs, want) {
		t.Errorf("attrs = %v, want %v", call.attrs, want)
	}
	if call.version != "gab/configurator" || !call.wifi.NoWifi {
		t.Errorf("version %q, wifi %+v; want gab/configurator and --no-wifi", call.version, call.wifi)
	}
}

func TestInstallBuildAttrUsageErrors(t *testing.T) {
	calls := stubLiteBuild(t)
	for _, args := range [][]string{
		{"--device-type", "raspberry-pi-5", "--no-wifi", "--build-attr", "board=x"},
		{"/nonexistent/wendy-test.img", "/dev/wendy-test-missing", "--build-attr", "board=x"},
		{"--device-type", "esp32", "--no-wifi", "--build-attr", "board"},
		{"--device-type", "esp32", "--no-wifi", "--build-attr", "board="},
		{"--device-type", "esp32", "--no-wifi", "--build-attr", "board=a", "--build-attr", "board=b"},
		{"--device-type", "esp32", "--no-wifi", "--pr", "3"},
		{"--device-type", "esp32", "--no-wifi", "--drive", "/dev/wendy-test-missing"},
		{"--device-type", "esp32", "--no-wifi", "--driver", "x", "--device-name", "lab-esp"},
		{"--device-type", "esp32", "--no-wifi", "--json"},
	} {
		if err := runInstallCmd(args...); err == nil || !IsUsageError(err) {
			t.Errorf("%v: %v; want a usage error", args, err)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("Wendy Lite build runs: %d, want none", len(*calls))
	}
}

// Without a terminal, the Wendy Lite build needs a Wi-Fi choice, like the
// other installs.
func TestInstallESP32WithoutTerminalNeedsWiFi(t *testing.T) {
	calls := stubLiteBuild(t)
	err := runInstallCmd("--device-type", "esp32", "--build-attr", "board=x")
	if !errors.Is(err, errWiFiUndecided) || !IsUsageError(err) {
		t.Fatalf("%v; want errWiFiUndecided as a usage error", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Wendy Lite build runs: %d, want none", len(*calls))
	}
}

const buildAttrConf = `
attributes:
  board: {type: string, enum: [a, b]}
  tunnel: {type: boolean}
display:
  board: {title: Board}
  tunnel: {title: Cloud tunnel}
rules:
  - if: tunnel
    sdkconfig: CONFIG_TUNNEL=y
`

func TestLiteBuildGiven(t *testing.T) {
	m, err := litebuild.Load([]byte(buildAttrConf), "test")
	if err != nil {
		t.Fatal(err)
	}
	given, err := liteBuildGiven(m, map[string]string{"board": "b", "tunnel": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if given["board"] != "b" || given["tunnel"] != true {
		t.Errorf("given = %v", given)
	}
	for attrs, want := range map[string]string{
		"colour=red": `--build-attr colour=red: unknown attribute "colour" (attributes: board, tunnel)`,
		"board=c":    "--build-attr board=c: board: c is not one of: a, b",
	} {
		name, value, _ := strings.Cut(attrs, "=")
		_, err := liteBuildGiven(m, map[string]string{name: value})
		if err == nil || err.Error() != want || !IsUsageError(err) {
			t.Errorf("%s: %v; want the usage error %q", attrs, err, want)
		}
	}
}

func TestNoTerminalQuestion(t *testing.T) {
	m, err := litebuild.Load([]byte(buildAttrConf), "test")
	if err != nil {
		t.Fatal(err)
	}
	q := litebuild.Question{Attr: m.Attributes["tunnel"], Options: []any{false, true}}
	p := &noTerminalPrompter{}
	_, err = p.Confirm(q)
	want := `no terminal to ask "Cloud tunnel": pass --build-attr tunnel=<value> (allowed: false, true)`
	if err == nil || err.Error() != want || !IsUsageError(err) {
		t.Errorf("Confirm: %v; want the usage error %q", err, want)
	}
}

func TestPlainFlashProgress(t *testing.T) {
	var out strings.Builder
	progress := plainFlashProgress(&out)
	for _, pct := range []float64{0.01, 0.05, 0.10, 0.11, 0.35, 0.36, 0.99, 1} {
		progress(pct)
	}
	if want := "  10%\n  30%\n  90%\n  100%\n"; out.String() != want {
		t.Errorf("progress lines = %q, want %q", out.String(), want)
	}
}
