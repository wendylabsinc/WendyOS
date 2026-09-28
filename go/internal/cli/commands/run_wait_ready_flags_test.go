package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// PR #1882 (agent-verified deployments) defines these flags; this CLI-side
// implementation must keep the same names, types, and defaults so that work
// can replace it without changing the interface.
func TestReadinessFlagsMatchVerifiedDeploymentInterface(t *testing.T) {
	cmd := newRunCmd()
	for _, tc := range []struct{ name, typ, def string }{
		{"wait-ready", "bool", "false"},
		{"readiness-timeout", "duration", "0s"},
	} {
		f := cmd.Flags().Lookup(tc.name)
		if f == nil {
			t.Fatalf("--%s is not defined", tc.name)
		}
		if f.Value.Type() != tc.typ || f.DefValue != tc.def {
			t.Errorf("--%s: type %q default %q, want %q %q", tc.name, f.Value.Type(), f.DefValue, tc.typ, tc.def)
		}
	}
	if got := cmd.Flags().Lookup("readiness-timeout").Usage; got != "Override the readiness deadline (whole seconds, 1s to 1h)" {
		t.Errorf("--readiness-timeout usage = %q", got)
	}
}

func TestValidateReadinessOptionsMatchesVerifiedDeploymentRules(t *testing.T) {
	const badTimeout = "--readiness-timeout must be a whole number of seconds between 1s and 1h"
	const deployOnly = "--deploy only creates containers; omit --deploy to use readiness verification"
	for _, tc := range []struct {
		name string
		opts runOptions
		want string
	}{
		{"defaults", runOptions{}, ""},
		{"one second", runOptions{readinessTimeout: time.Second}, ""},
		{"one hour", runOptions{readinessTimeout: time.Hour}, ""},
		{"fractional", runOptions{readinessTimeout: 1500 * time.Millisecond}, badTimeout},
		{"too short", runOptions{readinessTimeout: 500 * time.Millisecond}, badTimeout},
		{"too long", runOptions{readinessTimeout: time.Hour + time.Second}, badTimeout},
		{"deploy wait", runOptions{deploy: true, waitReady: true}, deployOnly},
		{"deploy timeout", runOptions{deploy: true, readinessTimeout: time.Minute}, deployOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReadinessOptions(tc.opts)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateWaitReadyFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       runOptions
		watch, hil bool
		want       string
	}{
		{name: "detached wait", opts: runOptions{detach: true, waitReady: true, readinessTimeout: time.Minute}},
		{name: "attached wait", opts: runOptions{waitReady: true}},
		{name: "attached timeout only", opts: runOptions{readinessTimeout: time.Minute}},
		{name: "watch timeout only", opts: runOptions{readinessTimeout: time.Minute}, watch: true},
		{name: "watch wait", opts: runOptions{waitReady: true}, watch: true, want: "--watch"},
		{name: "hil", opts: runOptions{waitReady: true}, hil: true, want: "--hil"},
		{name: "detached timeout without wait", opts: runOptions{detach: true, readinessTimeout: time.Minute}, want: "needs --wait-ready"},
		{name: "deploy", opts: runOptions{deploy: true, waitReady: true}, want: "--deploy only creates containers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWaitReadyFlags(tc.opts, tc.watch, tc.hil)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || ErrorClass(err) != "config_invalid" {
				t.Fatalf("err = %v (class %q), want config_invalid containing %q", err, ErrorClass(err), tc.want)
			}
		})
	}
}

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRejectUnsupportedWaitReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		cfg   *appconfig.AppConfig
		want  string
	}{
		{name: "dockerfile", files: map[string]string{"Dockerfile": "FROM scratch\n"}, cfg: &appconfig.AppConfig{AppID: "a"}},
		{name: "python", files: map[string]string{"requirements.txt": "flask\n"}, cfg: &appconfig.AppConfig{AppID: "a"}},
		{name: "swift with dockerfile", files: map[string]string{"Package.swift": "// swift\n", "Dockerfile": "FROM scratch\n"}, cfg: &appconfig.AppConfig{AppID: "a"}},
		{name: "host swift", files: map[string]string{"Package.swift": "// swift\n"}, cfg: &appconfig.AppConfig{AppID: "a"}, want: "Swift packages"},
		{name: "services", files: map[string]string{"Dockerfile": "FROM scratch\n"}, cfg: &appconfig.AppConfig{AppID: "a", Services: map[string]*appconfig.ServiceConfig{"api": {}}}, want: "multi-service"},
		{name: "native command", files: map[string]string{}, cfg: &appconfig.AppConfig{AppID: "a", Run: &appconfig.RunConfig{Command: "./serve"}}, want: "native commands"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeProject(t, tc.files)
			if err := rejectUnsupportedWaitReady(dir, tc.cfg, runOptions{}); err != nil {
				t.Fatalf("without --wait-ready: %v", err)
			}
			err := rejectUnsupportedWaitReady(dir, tc.cfg, runOptions{waitReady: true})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || ErrorClass(err) != "config_invalid" {
				t.Fatalf("err = %v, want config_invalid containing %q", err, tc.want)
			}
		})
	}
}

// These checks all run before any device is resolved or image built.
func TestRunCommandRejectsWaitReadyEarly(t *testing.T) {
	compose := writeProject(t, map[string]string{"docker-compose.yml": "services:\n  web:\n    image: nginx\n"})
	if err := runCommand(context.Background(), runOptions{prefix: compose, yes: true, detach: true, waitReady: true}); err == nil || !strings.Contains(err.Error(), "Compose projects") {
		t.Fatalf("compose: err = %v", err)
	}
	docker := writeProject(t, map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"a","version":"1.0.0"}`})
	if err := runCommand(context.Background(), runOptions{prefix: docker, yes: true, detach: true, waitReady: true, buildHost: "spark"}); err == nil || !strings.Contains(err.Error(), "--build-host") {
		t.Fatalf("build host: err = %v", err)
	}
	// Multi-service is only knowable once wendy.json is loaded (unlike Compose,
	// which is detected from the project directory alone), so this guards
	// against rejectUnsupportedWaitReady being wired in only after
	// resolveRunTarget: that would let a multi-service --wait-ready run reach a
	// device picker or cloud tunnel before being told the combination isn't
	// supported.
	multiService := writeProject(t, map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"a","services":{"api":{"context":"api"}}}`})
	if err := runCommand(context.Background(), runOptions{prefix: multiService, yes: true, detach: true, waitReady: true}); err == nil || !strings.Contains(err.Error(), "multi-service") || ErrorClass(err) != "config_invalid" {
		t.Fatalf("multi-service: err = %v", err)
	}
	// Xcode/host-only-Swift are knowable from the filesystem alone, but with NO
	// wendy.json yet (a common first run) runCommand's cfgMissing branch
	// resolves/connects to a device before wendy.json (and so appCfg) is ever
	// loaded. Use an already-cancelled context: if the early, project-type-only
	// rejectUnsupportedWaitReady(cwd, nil, opts) call regressed, this falls
	// through to that device-resolving preflight, which — given a cancelled
	// context — fails fast with a context/device error rather than hanging on
	// the network or an interactive prompt, so the test still fails quickly and
	// deterministically (by construction) instead of by timing out.
	swiftOnly := writeProject(t, map[string]string{"Package.swift": "// swift\n"})
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runCommand(cancelledCtx, runOptions{prefix: swiftOnly, yes: true, detach: true, waitReady: true}); err == nil || !strings.Contains(err.Error(), "Swift packages") || ErrorClass(err) != "config_invalid" {
		t.Fatalf("swift, no wendy.json yet: err = %v", err)
	}
	if err := runWithProvider(context.Background(), nil, models.ExternalDevice{}, docker, "a", nil, runOptions{waitReady: true}); err == nil || ErrorClass(err) != "config_invalid" {
		t.Fatalf("provider: err = %v", err)
	}
}

// `--detach --wait-ready --json` promises exactly one JSON object on stdout,
// so the run's non-interactive build progress (stdout by default) must move.
func TestWaitReadyJSONStdoutGuard(t *testing.T) {
	previousJSON, previousOut := jsonOutput, buildProgressOut
	t.Cleanup(func() { jsonOutput, buildProgressOut = previousJSON, previousOut })
	buildProgressOut = os.Stdout

	jsonOutput = true
	restore := waitReadyJSONStdoutGuard(runOptions{detach: true, waitReady: true})
	if buildProgressOut != os.Stderr {
		t.Fatalf("build progress writer = %v, want os.Stderr", buildProgressOut)
	}
	restore()
	if buildProgressOut != os.Stdout {
		t.Fatal("build progress writer was not restored")
	}
	for _, tc := range []struct {
		json bool
		opts runOptions
	}{
		{json: false, opts: runOptions{detach: true, waitReady: true}},
		{json: true, opts: runOptions{detach: true}},
		{json: true, opts: runOptions{waitReady: true}},
	} {
		jsonOutput = tc.json
		waitReadyJSONStdoutGuard(tc.opts)()
		if buildProgressOut != os.Stdout {
			t.Fatalf("json=%v opts=%+v moved build progress", tc.json, tc.opts)
		}
	}
}

// checkFailedWaitReadyObject asserts stdout is exactly one "failed" --wait-ready
// object for runErr, naming app and device when known and omitting them when
// not.
func checkFailedWaitReadyObject(t *testing.T, stdout string, runErr error, app, device string) {
	t.Helper()
	if runErr == nil {
		t.Fatal("the run succeeded; want a failure")
	}
	got := decodeOneJSONObject(t, stdout)
	if got["status"] != waitReadyStatusFailed || got["readiness"] != readinessNotChecked || got["message"] != runErr.Error() {
		t.Fatalf("object = %v, want status failed / readiness not_checked / message %q", got, runErr.Error())
	}
	for key, want := range map[string]string{"app": app, "device": device} {
		value, ok := got[key]
		switch {
		case want == "" && ok:
			t.Errorf("object carries %s %v; it was not known", key, value)
		case want != "" && value != want:
			t.Errorf("%s = %v, want %q", key, value, want)
		}
	}
	for _, key := range []string{"exit_code", "termination_reason", "url"} {
		if _, ok := got[key]; ok {
			t.Errorf("a failed object carries %q: %v", key, got)
		}
	}
}

// A `--json --detach --wait-ready` run promises exactly one JSON object on
// stdout, failures included — also when it fails before the wait: config,
// device, build, push, or start. That object has status "failed" and names
// the app and device once the run knows them.
func TestWaitReadyJSONFailureBeforeTheWaitPrintsOneFailedObject(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	opts := runOptions{yes: true, detach: true, waitReady: true}
	docker := writeProject(t, map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"a","version":"1.0.0"}`})
	multiService := writeProject(t, map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"a","services":{"api":{"context":"api"}}}`})
	for _, tc := range []struct {
		name        string
		run         func(context.Context) error
		app, device string
	}{
		{name: "build failure, nothing known yet", run: func(context.Context) error {
			return commandErrorf(errBuildFailed, "build failed: exit status 1")
		}},
		{name: "rejected once wendy.json is loaded", app: "a", run: func(ctx context.Context) error {
			o := opts
			o.prefix = multiService
			return runCommand(ctx, o)
		}},
		{name: "failure after the device resolved", app: "a", device: "dev.local", run: func(ctx context.Context) error {
			o := opts
			o.prefix = docker
			// A resolved target with no provisioning service: the run fails
			// at cloud registration, after it knows the app and device.
			o.watchTarget = &SelectedDevice{Agent: &grpcclient.AgentConnection{Host: "dev.local"}}
			return runCommand(ctx, o)
		}},
		{name: "on the device, no device name", app: "a", run: func(ctx context.Context) error {
			noteWaitReadyApp(ctx, "a")
			noteWaitReadyDevice(ctx, &grpcclient.AgentConnection{Host: "unix:/run/wendy/agent.sock"})
			return commandErrorf(errContainerStartFailed, "agent closed the stream before confirming the container started")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stdout := captureStdout(t, func() {
				err = runReportingWaitReadyFailure(context.Background(), opts, tc.run)
			})
			checkFailedWaitReadyObject(t, stdout, err, tc.app, tc.device)
		})
	}
}

// RunE applies the rule from its first check: a flag combination rejected
// before anything runs is still a --wait-ready JSON run's failure.
func TestRunCmdWaitReadyJSONValidationFailurePrintsOneFailedObject(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	cmd := newRunCmd()
	cmd.SetArgs([]string{"--detach", "--wait-ready", "--readiness-timeout", "1500ms"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	var err error
	stdout := captureStdout(t, func() { err = cmd.Execute() })
	if ErrorClass(err) != "config_invalid" {
		t.Fatalf("err = %v (class %q), want config_invalid", err, ErrorClass(err))
	}
	checkFailedWaitReadyObject(t, stdout, err, "", "")
}

// Once the wait printed its outcome, the run's error is that outcome's: no
// second object.
func TestWaitReadyJSONPrintedOutcomeIsTheOnlyObject(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true
	opts := runOptions{detach: true, waitReady: true, readinessTimeout: time.Second}
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{runningSnapshot(0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)}}
	conn := &grpcclient.AgentConnection{Host: "dev", ContainerService: fake}
	var err error
	stdout := captureStdout(t, func() {
		err = runReportingWaitReadyFailure(context.Background(), opts, func(ctx context.Context) error {
			return waitReadyAfterDetachedStart(ctx, conn, &appconfig.AppConfig{AppID: "app"}, opts)
		})
	})
	if ErrorClass(err) != "app_crashed" {
		t.Fatalf("err = %v, want app_crashed", err)
	}
	if got := decodeOneJSONObject(t, stdout); got["status"] != waitReadyStatusCrashed {
		t.Fatalf("object = %v, want the crashed outcome", got)
	}
}

// Ctrl-C and SIGTERM print no object, and neither does a run that is not a
// JSON-mode detached --wait-ready run.
func TestWaitReadyJSONFailedObjectOnlyForNonCancelJSONDetachedWaits(t *testing.T) {
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	buildErr := commandErrorf(errBuildFailed, "build failed: exit status 1")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	sigterm := func(sig os.Signal) func(context.Context) error {
		return func(ctx context.Context) error {
			sigCh := make(chan os.Signal, 1)
			sigCh <- sig
			return runWithInterruptChannel(ctx, sigCh, func(context.Context) error { return buildErr })
		}
	}
	jsonWait := runOptions{detach: true, waitReady: true}
	for _, tc := range []struct {
		name string
		json bool
		opts runOptions
		ctx  context.Context
		run  func(context.Context) error
	}{
		{name: "Ctrl-C", json: true, opts: jsonWait, ctx: context.Background(), run: sigterm(os.Interrupt)},
		{name: "SIGTERM", json: true, opts: jsonWait, ctx: context.Background(), run: sigterm(syscall.SIGTERM)},
		{name: "bare ErrUserCancelled", json: true, opts: jsonWait, ctx: context.Background(), run: func(context.Context) error { return ErrUserCancelled }},
		{name: "signal context already cancelled", json: true, opts: jsonWait, ctx: cancelled, run: func(ctx context.Context) error { return fmt.Errorf("building: %w", ctx.Err()) }},
		{name: "success", json: true, opts: jsonWait, ctx: context.Background(), run: func(context.Context) error { return nil }},
		{name: "not JSON", opts: jsonWait, ctx: context.Background(), run: func(context.Context) error { return buildErr }},
		{name: "not detached", json: true, opts: runOptions{waitReady: true}, ctx: context.Background(), run: func(context.Context) error { return buildErr }},
		{name: "no --wait-ready", json: true, opts: runOptions{detach: true}, ctx: context.Background(), run: func(context.Context) error { return buildErr }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jsonOutput = tc.json
			stdout := captureStdout(t, func() { _ = runReportingWaitReadyFailure(tc.ctx, tc.opts, tc.run) })
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing", stdout)
			}
		})
	}
}
