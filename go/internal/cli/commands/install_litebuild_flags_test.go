//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
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

// stubLiteBuild replaces the Wendy Lite build with a recorder of its
// options, and fails the test on any manifest request.
func stubLiteBuild(t *testing.T) *[]liteBuildOptions {
	t.Helper()
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()
	serveManifests(t, nil)
	var calls []liteBuildOptions
	prev := runLiteBuildFn
	t.Cleanup(func() { runLiteBuildFn = prev })
	runLiteBuildFn = func(_ context.Context, o liteBuildOptions) error {
		calls = append(calls, o)
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
	given, err := liteBuildGiven(m, map[string]string{"board": "b", "tunnel": "yes"}, nil)
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
		_, err := liteBuildGiven(m, map[string]string{name: value}, nil)
		if err == nil || err.Error() != want || !IsUsageError(err) {
			t.Errorf("%s: %v; want the usage error %q", attrs, err, want)
		}
	}

	// The required values come under the flags; the ones that the master
	// configuration does not accept are skipped.
	given, err = liteBuildGiven(m, map[string]string{"board": "a"},
		map[string]string{"board": "b", "tunnel": "true", "chip": "esp32c61"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"board": "a", "tunnel": true}; !maps.Equal(given, want) {
		t.Errorf("given = %v, want %v", given, want)
	}
	if given, err := liteBuildGiven(m, nil, map[string]string{"board": "z"}); err != nil || len(given) != 0 {
		t.Errorf("invalid required value: %v, %v; want it skipped", given, err)
	}
}

func TestLiteCatalogInstallEnabled(t *testing.T) {
	for value, want := range map[string]bool{"": false, "1": true, "true": true, "0": false, "nope": false} {
		t.Setenv(liteCatalogInstallEnv, value)
		if got := liteCatalogInstallEnabled(); got != want {
			t.Errorf("%s=%q: %v, want %v", liteCatalogInstallEnv, value, got, want)
		}
	}
}

// stubLiteCatalog replaces the published-firmware catalog with one ESP32-C6
// board, and counts the fetches.
func stubLiteCatalog(t *testing.T) *int {
	t.Helper()
	fetches := 0
	prev := fetchLiteCatalog
	t.Cleanup(func() { fetchLiteCatalog = prev })
	fetchLiteCatalog = func(bool) ([]WendyLiteBoard, []WendyLiteTarget) {
		fetches++
		return []WendyLiteBoard{{Board: "xiao_c6", Target: "esp32c6"}},
			[]WendyLiteTarget{{Name: "esp32c6", DisplayName: "ESP32-C6"}}
	}
	return &fetches
}

func TestLitePickerItems(t *testing.T) {
	fetches := stubLiteCatalog(t)
	t.Setenv(liteCatalogInstallEnv, "")
	items, boards := litePickerItems(false)
	if len(items) != 1 || items[0].Value != liteBuildValue || len(boards) != 0 || *fetches != 0 {
		t.Fatalf("catalog off: items %+v, boards %+v, %d fetches; want only the ESP32 platform entry and no fetch", items, boards, *fetches)
	}

	t.Setenv(liteCatalogInstallEnv, "1")
	items, boards = litePickerItems(false)
	var values []any
	for _, item := range items {
		values = append(values, item.Value)
	}
	if want := []any{"wlite_target_esp32c6", liteBuildValue}; len(values) != 2 || values[0] != want[0] || values[1] != want[1] {
		t.Errorf("catalog on: values %v, want %v", values, want)
	}
	if len(boards) != 1 || boards[0].Board != "xiao_c6" {
		t.Errorf("catalog on: boards %+v", boards)
	}
}

func TestLiteBuildOptionsFor(t *testing.T) {
	base := liteBuildOptions{version: "1.2.3", attrs: map[string]string{"board": "x"}}
	if o, ok := liteBuildOptionsFor(liteBuildValue, base); !ok || o.required != nil || o.version != "1.2.3" {
		t.Errorf("ESP32 platform: %+v, %v", o, ok)
	}
	o, ok := liteBuildOptionsFor(liteBuildS31Value, base)
	if !ok || o.required[litebuild.Chip] != "esp32s31" || o.attrs["board"] != "x" {
		t.Errorf("detected S31: %+v, %v; want the S31 chip required", o, ok)
	}
	if base.required != nil {
		t.Errorf("the caller's options changed: %+v", base)
	}
	if _, ok := liteBuildOptionsFor("raspberry-pi-5", base); ok {
		t.Error("raspberry-pi-5 is not a Wendy Lite build")
	}
}

// The incompatible-version error keeps the user's wording; wendy run
// replaces the --version hint, which it has no flag for.
func TestLiteBuildVersionHint(t *testing.T) {
	src := litebuild.Source{Name: "0.15.0", Version: "0.15.0", Commit: strings.Repeat("a", 40)}
	err := liteBuildOptions{}.withVersionHint(liteBuildIncompatible(src))
	if want := "Wendy Lite 0.15.0 is not compatible with this version of the CLI. Specify a Wendy Lite version with --version"; err.Error() != want {
		t.Errorf("install: %q, want %q", err, want)
	}
	err = liteBuildOptions{versionHint: liteRunBuildVersionHint}.withVersionHint(fmt.Errorf("opening: %w", liteBuildIncompatible(src)))
	if want := "Wendy Lite 0.15.0 is not compatible with this version of the CLI. " + liteRunBuildVersionHint; err.Error() != want {
		t.Errorf("run: %q, want %q", err, want)
	}

	// A newer master configuration format: update the CLI, or the hint.
	_, loadErr := litebuild.Load([]byte(`format_version: "2.0"`), "test")
	var formatErr *litebuild.FormatVersionError
	if !errors.As(loadErr, &formatErr) {
		t.Fatalf("format 2.0: %v, want a format error", loadErr)
	}
	newer := &liteBuildIncompatibleError{src: src, err: loadErr}
	for _, tc := range []struct{ hint, want string }{
		{"", "Wendy Lite 0.15.0 needs a newer version of the CLI. Update the CLI or specify a Wendy Lite version with --version"},
		{liteRunBuildVersionHint, "Wendy Lite 0.15.0 needs a newer version of the CLI. Update the CLI or install a Wendy Lite version with 'wendy install --device-type esp32 --version <version>'"},
		{liteSimulatorVersionHint("sim", "wendy-lite-native"), "Wendy Lite 0.15.0 needs a newer version of the CLI. Update the CLI or specify a Wendy Lite version: wendy vm create sim --profile wendy-lite-native --version <version>"},
	} {
		err := liteBuildOptions{versionHint: tc.hint}.withVersionHint(newer)
		if err.Error() != tc.want {
			t.Errorf("hint %q: %q, want %q", tc.hint, err, tc.want)
		}
		if !errors.As(err, &formatErr) {
			t.Errorf("hint %q: the error does not wrap the format error", tc.hint)
		}
	}

	// A master configuration that the CLI rejects: verify the CLI, or the
	// hint.
	_, loadErr = litebuild.Load([]byte("format_version: \"1.0\"\nattributes: {}\nnew_section: {}\n"), "test")
	var confErr *litebuild.ConfError
	if !errors.As(loadErr, &confErr) {
		t.Fatalf("unknown key: %v, want a ConfError", loadErr)
	}
	rejected := &liteBuildIncompatibleError{src: src, err: loadErr}
	for _, tc := range []struct{ hint, want string }{
		{"", "Wendy Lite 0.15.0 is not compatible with this version of the CLI. Verify that the CLI is up-to-date or specify a Wendy Lite version with --version"},
		{liteRunBuildVersionHint, "Wendy Lite 0.15.0 is not compatible with this version of the CLI. Verify that the CLI is up-to-date or install a Wendy Lite version with 'wendy install --device-type esp32 --version <version>'"},
		{liteSimulatorVersionHint("sim", "wendy-lite-native"), "Wendy Lite 0.15.0 is not compatible with this version of the CLI. Verify that the CLI is up-to-date or specify a Wendy Lite version: wendy vm create sim --profile wendy-lite-native --version <version>"},
	} {
		err := liteBuildOptions{versionHint: tc.hint}.withVersionHint(rejected)
		if err.Error() != tc.want {
			t.Errorf("hint %q: %q, want %q", tc.hint, err, tc.want)
		}
		if !errors.As(err, &confErr) {
			t.Errorf("hint %q: the error does not wrap the configuration error", tc.hint)
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
