package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/cliupdate"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func cliUpdaterTestDeps(t *testing.T) (cliUpdaterDeps, *[]string) {
	t.Helper()
	old := version.Version
	version.Version = "2026.09.16-025644"
	t.Cleanup(func() { version.Version = old })
	var events []string
	d := cliUpdaterDeps{
		check: func(context.Context) (string, error) {
			events = append(events, "check")
			return "2026.10.05-134249", nil
		},
		detect: func(context.Context) (cliupdate.Plan, error) {
			return cliupdate.Plan{Method: "brew", Channel: "stable", Supported: true, Commands: [][]string{{"brew", "upgrade", "wendylabsinc/tap/wendy"}}}, nil
		},
		apply: func(_ context.Context, _ cliupdate.Plan, current, target string, _ io.Reader, _, _ io.Writer) (cliupdate.Result, error) {
			events = append(events, "apply:"+target)
			return cliupdate.Result{Version: "2026.10.05-134249", Executable: "/opt/homebrew/bin/wendy"}, nil
		},
		interactive: func() bool { return false },
		isCI:        func() bool { return false },
		initialize:  func(*cobra.Command) {},
		confirm:     func(*cobra.Command) (bool, error) { t.Fatal("unexpected prompt"); return false, nil },
		track:       func(_ *cobra.Command, event, outcome, code, method string) { events = append(events, event) },
		record:      func(v string) error { events = append(events, "record:"+v); return nil },
	}
	return d, &events
}

func executeCLIUpdater(t *testing.T, d cliUpdaterDeps, args ...string) (string, string, error) {
	t.Helper()
	cmd := newCLIUpdateCmdWithDeps(d)
	cmd.Flags().Bool("json", false, "")
	var out, errout bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errout)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), errout.String(), err
}

func TestCLIUpdateCheckIsFreshAndReadOnly(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.initialize = func(*cobra.Command) { t.Fatal("read-only check initialized analytics") }
	out, _, err := executeCLIUpdater(t, d, "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report cliUpdateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !report.UpdateAvailable || report.LatestStableVersion != "2026.10.05-134249" {
		t.Fatalf("report=%+v", report)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("check changed state: %v", *events)
	}
}

func TestCLIUpdateCINeverPrompts(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.isCI = func() bool { return true }
	d.interactive = func() bool { return true }
	_, _, err := executeCLIUpdater(t, d)
	if err == nil || !strings.Contains(err.Error(), "--yes") || !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("err=%v events=%v", err, *events)
	}
}

func TestCLIUpdateCancellationIsNotAProductFailure(t *testing.T) {
	d, _ := cliUpdaterTestDeps(t)
	d.apply = func(context.Context, cliupdate.Plan, string, string, io.Reader, io.Writer, io.Writer) (cliupdate.Result, error) {
		return cliupdate.Result{}, context.Canceled
	}
	var observed []string
	d.track = func(_ *cobra.Command, event, outcome, code, _ string) {
		observed = append(observed, strings.Join([]string{event, outcome, code}, ":"))
	}
	_, _, err := executeCLIUpdater(t, d, "--yes")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(observed, []string{"cli_update_started:started:", "cli_update_failed:cancelled:user_cancelled"}) {
		t.Fatalf("err=%v observed=%v", err, observed)
	}
}

func TestCLIUpdateNeedsExplicitNoninteractiveConsent(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	_, _, err := executeCLIUpdater(t, d)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("ran without approval: %v", *events)
	}
}

func TestCLIUpdateYesVerifiesAndRecords(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	out, _, err := executeCLIUpdater(t, d, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"check", "cli_update_started", "apply:2026.10.05-134249", "cli_update_verified", "record:2026.10.05-134249"}
	if !reflect.DeepEqual(*events, want) {
		t.Fatalf("events=%v", *events)
	}
	var report cliUpdateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.VerifiedVersion != "2026.10.05-134249" || !report.RestartMCP || report.Status != "updated" {
		t.Fatalf("report=%+v", report)
	}
}

func TestCLIUpdateFailedDoesNotClaimSuccess(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.apply = func(context.Context, cliupdate.Plan, string, string, io.Reader, io.Writer, io.Writer) (cliupdate.Result, error) {
		return cliupdate.Result{}, errors.New("no new version on PATH")
	}
	out, _, err := executeCLIUpdater(t, d, "--yes")
	if err == nil || strings.Contains(out, "Updated and verified") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !reflect.DeepEqual(*events, []string{"check", "cli_update_started", "cli_update_failed"}) {
		t.Fatalf("events=%v", *events)
	}
}

func TestCLIUpdateNightlyPreservesChannel(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.check = func(context.Context) (string, error) {
		t.Error("nightly update requested the latest stable release")
		return "", errors.New("release service unavailable")
	}
	d.detect = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "brew", Channel: "nightly", Supported: true, Commands: [][]string{{"brew", "upgrade", "wendylabsinc/tap/wendy-nightly"}}}, nil
	}
	_, _, err := executeCLIUpdater(t, d, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cli_update_started", "apply:", "cli_update_verified"}
	if !reflect.DeepEqual(*events, want) {
		t.Fatalf("nightly update events=%v", *events)
	}
}

func TestCLIUpdateNightlyCheckIsFreshAndReadOnly(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.detect = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "brew", Channel: "nightly", Supported: true, Commands: [][]string{{"brew", "upgrade", "wendylabsinc/tap/wendy-nightly"}}}, nil
	}
	d.initialize = func(*cobra.Command) { t.Fatal("read-only check initialized analytics") }
	out, _, err := executeCLIUpdater(t, d, "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report cliUpdateReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "nightly_channel_not_checked" || report.UpdateAvailable || report.LatestStableVersion != "2026.10.05-134249" {
		t.Fatalf("report=%+v", report)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("nightly check changed state: %v", *events)
	}
}

func TestCLIUpdateDeclineDoesNotExecute(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.interactive = func() bool { return true }
	d.confirm = func(*cobra.Command) (bool, error) { return false, nil }
	_, _, err := executeCLIUpdater(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("events=%v", *events)
	}
}

func TestCLIUpdateUnsupportedInstallationIsGuidanceOnly(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	d.detect = func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "apt", Channel: "stable", Guidance: "Run sudo apt-get install --only-upgrade wendy"}, nil
	}
	_, _, err := executeCLIUpdater(t, d, "--yes")
	if err == nil || !strings.Contains(err.Error(), "apt-get") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("events=%v", *events)
	}
}

func TestCLIUpdateCheckFailureNeverMeansCurrent(t *testing.T) {
	for _, channel := range []string{"stable", "nightly"} {
		t.Run(channel, func(t *testing.T) {
			d, events := cliUpdaterTestDeps(t)
			d.detect = func(context.Context) (cliupdate.Plan, error) {
				return cliupdate.Plan{Method: "brew", Channel: channel, Supported: true}, nil
			}
			offline := errors.New("offline")
			d.check = func(context.Context) (string, error) { return "", offline }
			out, _, err := executeCLIUpdater(t, d, "--check", "--json")
			if !errors.Is(err, offline) || out != "" || len(*events) != 0 {
				t.Fatalf("out=%q err=%v events=%v", out, err, *events)
			}
		})
	}
}

func TestCLIUpdateCurrentVersionSkipsInstaller(t *testing.T) {
	d, events := cliUpdaterTestDeps(t)
	version.Version = "2026.10.05-134249"
	out, _, err := executeCLIUpdater(t, d, "--yes")
	if err != nil || !strings.Contains(out, "latest stable") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if !reflect.DeepEqual(*events, []string{"check"}) {
		t.Fatalf("events=%v", *events)
	}
}

func TestCLIUpdateInitializesTelemetryOnlyForExplicitExecution(t *testing.T) {
	d, _ := cliUpdaterTestDeps(t)
	initialized := 0
	d.initialize = func(*cobra.Command) { initialized++ }
	if _, _, err := executeCLIUpdater(t, d, "--yes"); err != nil {
		t.Fatal(err)
	}
	if initialized != 1 {
		t.Fatalf("explicit update initialized %d times", initialized)
	}
	d.initialize = func(*cobra.Command) { t.Fatal("ambient updater must not reinitialize root telemetry") }
	p, err := d.detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := performCLIUpdateWithPlan(cmd, d, p, "2026.10.05-134249"); err != nil {
		t.Fatal(err)
	}
}

// wendy update recognizes no updater for a CLI the Wendy plugin's launcher
// installed: it reports the plugin's advice and never runs an installer.
// Any other installation keeps what detection found.
func TestCLIUpdatePluginManagedCLIGetsPluginAdvice(t *testing.T) {
	home := isolateAIToolHome(t)
	managed := filepath.Join(home, ".wendy", "cli", "2026.09.16-025644", "wendy")
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := cliExecutable
	t.Cleanup(func() { cliExecutable = oldExe })
	direct := func(context.Context) (cliupdate.Plan, error) {
		return cliupdate.Plan{Method: "direct", Channel: "stable", Executable: managed, Guidance: "not owned by a recognized updater"}, nil
	}

	cliExecutable = func() (string, error) { return managed, nil }
	d, events := cliUpdaterTestDeps(t)
	d.detect = pluginAwareDetect(direct)
	out, _, err := executeCLIUpdater(t, d, "--check")
	if err != nil {
		t.Fatal(err)
	}
	advice := cliupdate.PluginAdvice(runtime.GOOS)
	if !strings.Contains(out, advice) || strings.Contains(out, "not owned") {
		t.Errorf("--check output lacks the plugin advice:\n%s", out)
	}
	if _, _, err := executeCLIUpdater(t, d, "--yes"); err == nil || err.Error() != advice {
		t.Errorf("update error = %v, want the plugin advice", err)
	}
	for _, event := range *events {
		if strings.HasPrefix(event, "apply") || event == "cli_update_started" {
			t.Errorf("a plugin-managed CLI ran the updater: %v", *events)
		}
	}

	cliExecutable = func() (string, error) { return filepath.Join(home, "homebrew", "bin", "wendy"), nil }
	if p, _ := pluginAwareDetect(direct)(context.Background()); p.Method != "direct" || p.Guidance != "not owned by a recognized updater" {
		t.Errorf("a CLI the plugin did not install got %+v", p)
	}
}
