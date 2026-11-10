package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func doctorTestDeps(t *testing.T) doctorDeps {
	t.Helper()
	dir := t.TempDir()
	return doctorDeps{
		homeDir: func() (string, error) { return dir, nil },
		getwd:   func() (string, error) { return dir, nil },
		getenv:  func(string) string { return "" },
		readJSON: func(string, any) error {
			return os.ErrNotExist
		},
		lookPath: func(name string) (string, error) { return "/private/bin/" + name, nil },
		run:      func(context.Context, string, ...string) error { return nil },
		firmware: func(string, string) error { return nil },
		validate: func(context.Context, wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error) {
			return &wendymcp.ProjectValidation{Valid: true, BuildType: "docker"}, nil
		},
		probe: func(context.Context, string) doctorTargetEvidence { return doctorTargetEvidence{} },
	}
}

func doctorFindCheck(t *testing.T, report doctorReport, id string) doctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing check %s", id)
	return doctorCheck{}
}

func doctorExecute(t *testing.T, deps doctorDeps, args ...string) (doctorReport, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "wendy", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().String("device", "", "")
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentPreRunE = func(*cobra.Command, []string) error { t.Fatal("root pre-run must not run"); return nil }
	root.PersistentPostRunE = func(*cobra.Command, []string) error { t.Fatal("root post-run must not run"); return nil }
	root.AddCommand(newDoctorCmdWithDeps(deps))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"doctor", "--report"}, args...))
	err := root.Execute()
	var report doctorReport
	if e := json.Unmarshal(stdout.Bytes(), &report); e != nil {
		t.Fatalf("report must be standalone JSON: %v; output=%q; error=%v", e, stdout.String(), err)
	}
	return report, stdout.String() + stderr.String(), err
}

func TestDoctorReportNeverCopiesPrivateDiagnostics(t *testing.T) {
	d := doctorTestDeps(t)
	secret := "private-secret-token"
	d.readJSON = func(string, any) error { return errors.New(secret + " unreadable /private/config.json") }
	d.run = func(context.Context, string, ...string) error { return errors.New(secret + " daemon output") }
	d.firmware = func(string, string) error { return errors.New(secret + " /private/firmware") }
	d.validate = func(context.Context, wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error) {
		return &wendymcp.ProjectValidation{
			Valid: false, ProjectPath: "/private/source", AppID: secret, BuildType: secret,
			Diagnostics:   []wendymcp.ProjectDiagnostic{{Severity: "error", Field: secret, Message: secret, Fix: secret}},
			Builds:        []wendymcp.ProjectBuild{{Service: secret, File: secret, Type: secret}},
			Compatibility: wendymcp.ProjectCompatibility{Status: secret},
		}, nil
	}
	d.probe = func(context.Context, string) doctorTargetEvidence {
		return doctorTargetEvidence{Reachable: true, AgentResponding: true}
	}
	report, output, err := doctorExecute(t, d, "--project", "/private/source", "--device", "private-device.local")
	if !errors.Is(err, errDoctorChecksFailed) || report.OK {
		t.Fatalf("required failures must return a failing exit: report=%+v error=%v", report, err)
	}
	for _, forbidden := range []string{secret, "/private", "private-device.local", "daemon output"} {
		if strings.Contains(output, forbidden) || strings.Contains(err.Error(), forbidden) {
			t.Errorf("report leaked %q", forbidden)
		}
	}
	if got := doctorFindCheck(t, report, "authentication").Status; got != "unknown" {
		t.Fatalf("responding agent does not prove auth validity: %s", got)
	}
}

func TestDoctorOptionalContextDoesNotFail(t *testing.T) {
	d := doctorTestDeps(t)
	d.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	d.firmware = func(string, string) error { return os.ErrNotExist }
	d.probe = func(context.Context, string) doctorTargetEvidence {
		t.Fatal("must not scan without target")
		return doctorTargetEvidence{}
	}
	d.validate = func(context.Context, wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error) {
		t.Fatal("must not validate an unrelated current directory")
		return nil, nil
	}
	report, _, err := doctorExecute(t, d)
	if err != nil || !report.OK {
		t.Fatalf("optional missing context should succeed: %v", err)
	}
	for _, id := range []string{"project", "selected_target", "authentication"} {
		if got := doctorFindCheck(t, report, id); got.Status != "unknown" || got.Required {
			t.Errorf("%s should be optional unknown: %+v", id, got)
		}
	}
}

func TestDoctorRequiredToolsAndLocalAuthentication(t *testing.T) {
	d := doctorTestDeps(t)
	d.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	d.run = func(context.Context, string, ...string) error { t.Fatal("must not run missing executable"); return nil }
	d.probe = func(context.Context, string) doctorTargetEvidence {
		t.Fatal("docker is not a device endpoint")
		return doctorTargetEvidence{}
	}
	report, _, err := doctorExecute(t, d, "--device", "docker")
	if !errors.Is(err, errDoctorChecksFailed) || report.OK {
		t.Fatal("missing Docker must fail for local Docker target")
	}
	if got := doctorFindCheck(t, report, "docker_executable"); !got.Required || got.Status != "fail" {
		t.Fatalf("Docker is required: %+v", got)
	}
	if got := doctorFindCheck(t, report, "authentication").Status; got != "not_required" {
		t.Fatalf("local Docker does not require Cloud auth: %s", got)
	}
}

func TestDoctorDoesNotCreateConfigOrRepairStoppedVM(t *testing.T) {
	d := doctorTestDeps(t)
	configDir := filepath.Join(t.TempDir(), "not-created")
	d.getenv = func(key string) string {
		if key == "WENDY_CONFIG_DIR" {
			return configDir
		}
		return ""
	}
	d.readJSON = doctorReadJSON
	d.probe = func(context.Context, string) doctorTargetEvidence {
		t.Fatal("stopped VM must not be probed")
		return doctorTargetEvidence{}
	}
	report, _, err := doctorExecute(t, d, "--device", "vm:stopped")
	if !errors.Is(err, errDoctorChecksFailed) || doctorFindCheck(t, report, "device_reachable").Code != "vm_not_running" {
		t.Fatalf("expected stopped VM failure, got %v", err)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("doctor created a configuration directory: %v", err)
	}
	stateDir := filepath.Join(configDir, "vms", "stopped")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"pid":123456789,"agentPort":0,"logPath":"/private/log"}`)
	statePath := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	doctorExecute(t, d, "--device", "vm:stopped")
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, state) {
		t.Fatalf("doctor changed stale VM state: %v", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("doctor created VM files or locks: %v, %v", entries, err)
	}
}

func TestDoctorOnlyUsesReadOnlyProbesAndTargetPrecedence(t *testing.T) {
	d := doctorTestDeps(t)
	d.readJSON = func(_ string, value any) error {
		return json.Unmarshal([]byte(`{"defaultDevice":"saved-private.local"}`), value)
	}
	d.getenv = func(key string) string {
		if key == "WENDY_DEVICE" {
			return "environment-private.local"
		}
		return ""
	}
	var calls [][]string
	d.run = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{filepath.Base(name)}, args...))
		return nil
	}
	var endpoints []string
	var endpointsMu sync.Mutex
	d.probe = func(_ context.Context, address string) doctorTargetEvidence {
		endpointsMu.Lock()
		endpoints = append(endpoints, address)
		endpointsMu.Unlock()
		return doctorTargetEvidence{Reachable: true, AgentResponding: true, NotProvisioned: true}
	}
	report, output, err := doctorExecute(t, d, "--device", "explicit-private.local")
	if err != nil || !report.OK {
		t.Fatal(err)
	}
	if want := [][]string{{"docker", "info"}, {"docker", "buildx", "version"}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("only read-only Docker commands are allowed: got %v", calls)
	}
	slices.Sort(endpoints)
	if !reflect.DeepEqual(endpoints, []string{"explicit-private.local:50051", "explicit-private.local:50052"}) {
		t.Fatalf("explicit device must override env: %v", endpoints)
	}
	if strings.Contains(output, "private.local") {
		t.Fatal("target leaked")
	}
	if doctorFindCheck(t, report, "authentication").Status != "not_required" {
		t.Fatal("unprovisioned local agent should not require Cloud auth")
	}
	endpoints = nil
	doctorExecute(t, d)
	slices.Sort(endpoints)
	if !reflect.DeepEqual(endpoints, []string{"environment-private.local:50051", "environment-private.local:50052"}) {
		t.Fatalf("env target not used: %v", endpoints)
	}
	// A defaultDevice left in config.json by an older CLI is not a target.
	endpoints = nil
	d.getenv = func(string) string { return "" }
	doctorExecute(t, d)
	if len(endpoints) != 0 {
		t.Fatalf("saved default device was probed: %v", endpoints)
	}
}

func TestDoctorDoesNotResolveCloudOrCreateSimulator(t *testing.T) {
	for _, target := range []string{"cloud:42", "sim", "simulator", "vm:../bad", "42", "42:50051"} {
		t.Run(target, func(t *testing.T) {
			d := doctorTestDeps(t)
			d.probe = func(context.Context, string) doctorTargetEvidence {
				t.Fatal("selector must not trigger network or VM startup")
				return doctorTargetEvidence{}
			}
			report, _, _ := doctorExecute(t, d, "--device", target)
			if got := doctorFindCheck(t, report, "authentication").Status; got != "unknown" {
				t.Fatalf("auth must be unknown: %s", got)
			}
		})
	}
}

func TestDoctorAddressesOnlyAcceptsDirectTargets(t *testing.T) {
	for input, want := range map[string][]string{
		"device.local":       {"device.local:50051", "device.local:50052"},
		"192.0.2.1":          {"192.0.2.1:50051", "192.0.2.1:50052"},
		"::1":                {"[::1]:50051", "[::1]:50052"},
		"127.0.0.1:54321":    {"127.0.0.1:54321"},
		"[::1]:54321":        {"[::1]:54321"},
		"device.local:50051": {"device.local:50051"},
		"device.local:65535": {"device.local:65535"},
	} {
		got, err := doctorAddresses(input)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("doctorAddresses(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "42", "42:50051", "host:0", "host:65536", "host:bad", ":50051", "https://device.local", "user@device.local", "device.local/path", "device.local\n"} {
		if _, err := doctorAddresses(input); err == nil {
			t.Errorf("accepted invalid or indirect selector %q", input)
		}
	}
}

func TestDoctorReachabilityIncludesProvisionedTargets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selector  string
		tls       string
		want      []string
		reachable bool
	}{
		{"hostname", "device.local", "device.local:50052", []string{"device.local:50051", "device.local:50052"}, true},
		{"IPv6", "::1", "[::1]:50052", []string{"[::1]:50051", "[::1]:50052"}, true},
		{"VM", "vm:provisioned", "127.0.0.1:51112", []string{"127.0.0.1:51111", "127.0.0.1:51112"}, true},
		{"explicit plaintext", "device.local:50051", "device.local:50052", []string{"device.local:50051"}, false},
		{"explicit TLS", "device.local:50052", "device.local:50052", []string{"device.local:50052"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := doctorTestDeps(t)
			d.readJSON = func(_ string, value any) error {
				if state, ok := value.(*vm.State); ok {
					state.AgentPort = 51111
					return nil
				}
				return os.ErrNotExist
			}
			var endpoints []string
			var mu sync.Mutex
			d.probe = func(_ context.Context, address string) doctorTargetEvidence {
				mu.Lock()
				endpoints = append(endpoints, address)
				mu.Unlock()
				return doctorTargetEvidence{Reachable: address == tc.tls}
			}
			report, _, err := doctorExecute(t, d, "--device", tc.selector)
			slices.Sort(endpoints)
			if !reflect.DeepEqual(endpoints, tc.want) {
				t.Fatalf("probed %v; want %v", endpoints, tc.want)
			}
			if tc.reachable {
				if err != nil || !report.OK || doctorFindCheck(t, report, "device_reachable").Status != "pass" {
					t.Fatalf("TLS connection must prove reachability: %+v, %v", report, err)
				}
			} else if !errors.Is(err, errDoctorChecksFailed) || doctorFindCheck(t, report, "device_reachable").Status != "fail" {
				t.Fatalf("explicit plaintext port must remain unreachable: %+v, %v", report, err)
			}
			for _, id := range []string{"wendy_agent", "authentication"} {
				if got := doctorFindCheck(t, report, id).Status; got != "unknown" {
					t.Errorf("TCP reachability must leave %s unknown; got %s", id, got)
				}
			}
		})
	}
}

func TestDoctorProbeTargetsShareDeadlineWithoutBlockingTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	plaintextStarted := make(chan struct{})
	started := time.Now()
	evidence := doctorProbeTargets(ctx, func(ctx context.Context, address string) doctorTargetEvidence {
		if address == "device.local:50051" {
			close(plaintextStarted)
			<-ctx.Done()
			return doctorTargetEvidence{}
		}
		<-plaintextStarted
		reachable := ctx.Err() == nil
		cancel()
		return doctorTargetEvidence{Reachable: reachable}
	}, []string{"device.local:50051", "device.local:50052"})
	if !evidence.Reachable || evidence.AgentResponding || evidence.NotProvisioned {
		t.Fatalf("silent plaintext endpoint prevented TLS reachability: %+v", evidence)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("probes exceeded the shared timeout")
	}
}

func TestDoctorProbeTargetKeepsTCPReachabilityWhenRPCFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// TCP accepts connections, but no unauthenticated RPC can complete.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	evidence := doctorProbeTarget(ctx, listener.Addr().String())
	if !evidence.Reachable || evidence.AgentResponding || evidence.NotProvisioned {
		t.Fatalf("TCP reachability must survive an RPC failure: %+v", evidence)
	}
}

func TestDoctorRecognizesProjectWithoutRunningHooks(t *testing.T) {
	d := doctorTestDeps(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-exist")
	config := `{"appId":"dev.private.project","platform":"darwin","run":{"command":"/usr/bin/touch","args":["` + marker + `"]}}`
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	d.getwd = func() (string, error) { return dir, nil }
	d.validate = validateMCPProject
	report, _, err := doctorExecute(t, d)
	if err != nil || doctorFindCheck(t, report, "project").Status != "pass" {
		t.Fatalf("static native project validation failed: %+v %v", report, err)
	}
	if doctorFindCheck(t, report, "docker_executable").Required {
		t.Fatal("native project should not require Docker")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("doctor executed project command")
	}
}

func TestDoctorDeadlineBoundsSubprocessAndValidation(t *testing.T) {
	for _, slow := range []string{"process", "validation"} {
		t.Run(slow, func(t *testing.T) {
			d := doctorTestDeps(t)
			args := []string{"--timeout", "20ms"}
			if slow == "process" {
				d.run = func(ctx context.Context, _ string, _ ...string) error { <-ctx.Done(); return ctx.Err() }
			} else {
				d.validate = func(ctx context.Context, _ wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				args = append(args, "--project", t.TempDir())
			}
			started := time.Now()
			report, _, err := doctorExecute(t, d, args...)
			if time.Since(started) > time.Second {
				t.Fatal("diagnostic timeout was not enforced")
			}
			if !errors.Is(err, errDoctorChecksFailed) || doctorFindCheck(t, report, "diagnostics").Code != "diagnostic_timeout" {
				t.Fatalf("expected timeout failure: %v", err)
			}
		})
	}
}

func TestDoctorReadJSONRejectsUnboundedInputs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := doctorReadJSON(path, &value); err == nil {
		t.Fatal("oversized JSON accepted")
	}
	if err := doctorReadJSON(dir, &value); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestDoctorRegisteredAndHumanOutputByDefault(t *testing.T) {
	root := NewRootCmd()
	registered, _, err := root.Find([]string{"doctor"})
	if err != nil || registered.Name() != "doctor" || registered.PersistentPreRunE == nil || registered.PersistentPostRunE == nil {
		t.Fatalf("doctor not registered with read-only hooks: %v", err)
	}
	cmd := newDoctorCmdWithDeps(doctorTestDeps(t))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "Wendy setup checks.") {
		t.Fatalf("default output must be human-readable: %q", output.String())
	}
}

func TestDoctorRealRootLeavesFreshInstallationUntouched(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config-not-created")
	t.Setenv("WENDY_CONFIG_DIR", configDir)
	t.Setenv("WENDY_DEVICE", "")
	t.Setenv("PATH", dir)
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"doctor", "--report"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("non-JSON output: %v", err)
	}
	if !report.OK || stderr.Len() != 0 {
		t.Fatalf("unexpected root output: ok=%v stderr=%q", report.OK, stderr.String())
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("root created config: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("doctor changed fresh installation state: %v %v", entries, err)
	}
}
