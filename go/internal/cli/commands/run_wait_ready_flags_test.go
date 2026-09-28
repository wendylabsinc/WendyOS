package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
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
