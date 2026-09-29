package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func runProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(`{"appId":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunWithoutConnectionOrDeviceIsNotConnected(t *testing.T) {
	s := New(&config.Config{DefaultDevice: "must-not-be-used"}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		t.Fatal("run must not start a child without a target")
		return "", false, nil
	}
	for _, args := range []map[string]any{
		{"project_path": runProject(t)},
		{"project_path": runProject(t), "device": ""},
	} {
		r, err := s.handleRun(context.Background(), callToolReq("run", args))
		if err != nil || !r.IsError {
			t.Fatalf("args %v: want tool error, got %v %v", args, r, err)
		}
		out := structuredMap(t, r)
		if out["error_code"] != string(errCodeNotConnected) || !strings.Contains(out["message"].(string), "device_connect") {
			t.Fatalf("args %v: got %v", args, out)
		}
	}
}

func TestRunTargetArgumentErrorsStayInvalidArgument(t *testing.T) {
	project := runProject(t)
	disconnected := New(&config.Config{}, nil)
	// A non-replayable live connection (the on-device agent socket).
	unreplayable := New(&config.Config{}, nil)
	unreplayable.SetConn(&grpcclient.AgentConnection{Host: "unix:/run/wendy/agent.sock"})
	for _, tc := range []struct {
		s    *mcpServer
		args map[string]any
	}{
		{disconnected, map[string]any{"project_path": project, "cloud_grpc": "other:443"}},
		{disconnected, map[string]any{"project_path": project, "device": "vm:a", "device_name": "robot"}},
		{disconnected, map[string]any{"project_path": project, "device": "  "}},
		{unreplayable, map[string]any{"project_path": project}},
	} {
		r, err := tc.s.handleRun(context.Background(), callToolReq("run", tc.args))
		if err != nil || !r.IsError || structuredMap(t, r)["error_code"] != string(errCodeInvalidArgument) {
			t.Fatalf("args %v: got %v %v", tc.args, r, err)
		}
	}
}

func TestRunFailureCodeFromFinalDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   errorCode
	}{
		{"building...\n✗ not logged in; run 'wendy auth login' first\n", errCodeAuthRequired},
		{"✗ Unauthorized. Run 'wendy auth login' with an account that can access this provisioned wendy-agent.", errCodeAuthRequired},
		{"✗ no login for cloud cloud.wendy.dev:443 organization 2; log in to that organization or choose another default", errCodeAuthRequired},
		{"✗ multiple auth sessions exist; pass --cloud-grpc or run 'wendy auth use' to choose a default", errCodeMultipleSessions},
		{"#5 ERROR: process \"/bin/sh -c make\" did not complete successfully: exit code: 2", errCodeInternal},
		{"", errCodeInternal},
		// Only the final diagnostic counts: an early build-log mention is not an auth failure.
		{"echo 'wendy auth login'\n" + strings.Repeat("step output\n", 200) + "✗ build failed", errCodeInternal},
	} {
		if got := runFailureCode(tc.output); got != tc.want {
			t.Errorf("runFailureCode(%.60q) = %s, want %s", tc.output, got, tc.want)
		}
	}
}

func TestRunAuthFailureReportsAuthRequired(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		return "✗ not logged in; run 'wendy auth login' first", false, errors.New("exit status 1")
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "robot.local:50051"}))
	if err != nil || !r.IsError {
		t.Fatalf("want tool error: %v %v", r, err)
	}
	out := structuredMap(t, r)
	if out["error_code"] != string(errCodeAuthRequired) || out["status"] != "failed" || !strings.Contains(out["output"].(string), "wendy auth login") {
		t.Fatalf("got %v", out)
	}
}

// Review focus: an explicit device that differs from the session deploys
// there, and the result says to connect to it before verifying; otherwise
// container_list would inspect the old device.
func TestRunExplicitDeviceOverridesSessionAndSaysToConnect(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.commandTarget = commandTarget{Device: "robot-a.local:50051", Transport: "direct"}
	var args []string
	s.runCommandFn = func(_ context.Context, a []string, _ commandTarget, _ int) (string, bool, error) {
		args = a
		return "ok", false, nil
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "robot-b.local:50051"}))
	if err != nil || r.IsError {
		t.Fatalf("run: %v %v", r, err)
	}
	if !strings.Contains(strings.Join(args, " "), "--device robot-b.local:50051") {
		t.Fatalf("explicit device not used: %v", args)
	}
	out := structuredMap(t, r)
	if target, ok := out["target"].(commandTarget); !ok || target.Device != "robot-b.local:50051" {
		t.Fatalf("result target = %v", out["target"])
	}
	next := out["suggested_next_step"].(string)
	for _, want := range []string{"robot-a.local:50051", `device_connect(address="robot-b.local:50051")`, "before container_list"} {
		if !strings.Contains(next, want) {
			t.Fatalf("suggested_next_step does not name %q: %s", want, next)
		}
	}
	// Deploying to the connected target itself needs no reconnect, and a
	// deploy with no session names the connect call for the deployed device.
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) { return "ok", false, nil }
	same, _ := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t)}))
	if next := structuredMap(t, same)["suggested_next_step"].(string); strings.Contains(next, "device_connect") {
		t.Fatalf("session target run asks to reconnect: %s", next)
	}
	fresh := New(&config.Config{}, nil)
	fresh.runCommandFn = s.runCommandFn
	cloud, _ := fresh.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device_name": "robot-c"}))
	if next := structuredMap(t, cloud)["suggested_next_step"].(string); !strings.Contains(next, `cloud_connect(device_name="robot-c")`) {
		t.Fatalf("cloud deploy without a session: %s", next)
	}
}

// Review focus: a relative project_path resolves against the MCP server's
// working directory and reaches the CLI as an absolute --prefix.
func TestRunResolvesRelativeProjectPath(t *testing.T) {
	project := runProject(t)
	t.Chdir(project)
	want, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, nil)
	var args []string
	s.runCommandFn = func(_ context.Context, a []string, _ commandTarget, _ int) (string, bool, error) {
		args = a
		return "ok", false, nil
	}
	if r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": ".", "device": "vm:test"})); err != nil || r.IsError {
		t.Fatalf("run: %v %v", r, err)
	}
	if !strings.Contains(strings.Join(args, " "), "--prefix "+want+" ") {
		t.Fatalf("relative project_path not made absolute: %v", args)
	}
}

// The CLI validates the project: compose projects have no wendy.json, and
// `wendy run --yes` sets up a first deploy itself. Only a path that is not an
// existing directory is rejected up front.
func TestRunLeavesProjectValidationToTheCLI(t *testing.T) {
	compose := t.TempDir()
	if err := os.WriteFile(filepath.Join(compose, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, nil)
	var args []string
	s.runCommandFn = func(_ context.Context, a []string, _ commandTarget, _ int) (string, bool, error) {
		args = a
		return "ok", false, nil
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": compose, "device": "vm:test", "build_type": "compose"}))
	if err != nil || r.IsError || !strings.Contains(strings.Join(args, " "), "--prefix "+compose+" ") {
		t.Fatalf("compose project without wendy.json was not deployed: %v %v %v", r, err, args)
	}
	file := filepath.Join(compose, "compose.yaml")
	for _, path := range []string{filepath.Join(compose, "missing"), file} {
		args = nil
		r, _ := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": path, "device": "vm:test"}))
		if !r.IsError || args != nil || structuredMap(t, r)["error_code"] != string(errCodeInvalidArgument) || !strings.Contains(structuredMap(t, r)["message"].(string), "directory") {
			t.Fatalf("project_path %s must be rejected as not a directory: %v", path, r)
		}
	}
}

// Review focus: a build that outlives timeout_seconds reports TIMEOUT, keeps
// the partial log, and warns that the container may already exist.
func TestRunTimeoutKeepsTailAndWarnsContainerMayExist(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(ctx context.Context, _ []string, _ commandTarget, _ int) (string, bool, error) {
		<-ctx.Done()
		return "#7 exporting layers", false, ctx.Err()
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "vm:test", "timeout_seconds": 1}))
	if err != nil || !r.IsError {
		t.Fatalf("want tool error: %v %v", r, err)
	}
	out := structuredMap(t, r)
	if out["error_code"] != string(errCodeTimeout) || out["output"] != "#7 exporting layers" || !strings.Contains(out["message"].(string), "may already have created") {
		t.Fatalf("got %v", out)
	}
}

// Review focus: an MCP server running inside a device container (admin
// entitlement, WENDY_AGENT_SOCKET) must not spawn a host-style deploy.
func TestRunRefusesInsideDeviceContainer(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "/run/wendy/agent.sock")
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		t.Fatal("run must not spawn a child inside a device container")
		return "", false, nil
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "vm:test"}))
	if err != nil || !r.IsError || structuredMap(t, r)["error_code"] != string(errCodeUnsupported) {
		t.Fatalf("got %v %v", r, err)
	}
}

// A cancelled request (client cancellation or MCP shutdown) is reported
// distinctly from a timeout.
func TestRunCancellationIsNotReportedAsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(runCtx context.Context, _ []string, _ commandTarget, _ int) (string, bool, error) {
		cancel()
		<-runCtx.Done()
		return "#3 building", false, runCtx.Err()
	}
	r, err := s.handleRun(ctx, callToolReq("run", map[string]any{"project_path": runProject(t), "device": "vm:test"}))
	if err != nil || !r.IsError {
		t.Fatalf("want tool error: %v %v", r, err)
	}
	if out := structuredMap(t, r); out["error_code"] != string(errCodeCancelled) || out["output"] != "#3 building" {
		t.Fatalf("got %v", out)
	}
}

// A comma makes `wendy run --device` deploy one build to several devices.
func TestRunRejectsDeviceLists(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		t.Fatal("a device list must not reach the CLI")
		return "", false, nil
	}
	for _, args := range []map[string]any{
		{"project_path": runProject(t), "device": "robot-a.local:50051,robot-b.local:50051"},
		{"project_path": runProject(t), "device_name": "robot-a,robot-b"},
	} {
		r, err := s.handleRun(context.Background(), callToolReq("run", args))
		if err != nil || !r.IsError || structuredMap(t, r)["error_code"] != string(errCodeInvalidArgument) {
			t.Fatalf("args %v: got %v %v", args, r, err)
		}
	}
}

// The spawned CLI must not answer a failed direct connect with a same-named
// cloud device (commands.resolveWithCloudFallback reads this variable).
func TestRunChildEnvironmentDisablesCloudFallback(t *testing.T) {
	base := []string{"PATH=/bin", "WENDY_BROKER_URL=inherited:443"}
	direct := runChildEnvironment(base, commandTarget{Device: "robot.local:50051", Transport: "direct"})
	if !slices.Equal(direct, []string{"PATH=/bin", "WENDY_BROKER_URL=inherited:443", "WENDY_RUN_NO_CLOUD_FALLBACK=1"}) {
		t.Fatalf("direct environment = %v", direct)
	}
	cloud := runChildEnvironment(base, commandTarget{Device: "robot", Transport: "cloud", Selector: "cloud://c:443/org/1/asset/2", BrokerURL: "relay:443"})
	if !slices.Equal(cloud, []string{"PATH=/bin", "WENDY_RUN_NO_CLOUD_FALLBACK=1", "WENDY_BROKER_URL=relay:443"}) {
		t.Fatalf("cloud environment = %v", cloud)
	}
}

// An attached run waits for readiness, opens a browser on this host, runs
// postStart hooks and streams logs until stopped, so through MCP it could only
// time out. run always detaches and no longer advertises detach.
func TestRunAlwaysDetaches(t *testing.T) {
	if _, ok := startedProtocolServer(t).ListTools()["run"].Tool.InputSchema.Properties["detach"]; ok {
		t.Fatal("run still advertises detach")
	}
	project := runProject(t)
	for _, tc := range []struct {
		args    map[string]any
		invalid bool
	}{
		{map[string]any{}, false},
		{map[string]any{"detach": true}, false},
		{map[string]any{"detach": false}, true},
		{map[string]any{"detach": "no"}, true},
	} {
		s := New(&config.Config{}, nil)
		var got []string
		s.runCommandFn = func(_ context.Context, a []string, _ commandTarget, _ int) (string, bool, error) {
			got = a
			return "ok", false, nil
		}
		args := map[string]any{"project_path": project, "device": "vm:test"}
		for k, v := range tc.args {
			args[k] = v
		}
		r, err := s.handleRun(context.Background(), callToolReq("run", args))
		if err != nil || r.IsError != tc.invalid {
			t.Fatalf("args %v: %v %v", tc.args, r, err)
		}
		if tc.invalid {
			if got != nil || structuredMap(t, r)["error_code"] != string(errCodeInvalidArgument) {
				t.Fatalf("args %v: %v", tc.args, r)
			}
		} else if !slices.Contains(got, "--detach") {
			t.Fatalf("args %v: CLI not detached: %v", tc.args, got)
		}
	}
}

// The spawned CLI resolves an explicit device directly (no cloud fallback), so
// a cloud device's bare name fails. The schema asks for device_list's
// selector, and a failed bare name points the agent at that selector or, for
// a row without one, cloud_connect.
func TestRunPointsCloudDeviceNamesAtCloudConnect(t *testing.T) {
	desc := startedProtocolServer(t).ListTools()["run"].Tool.InputSchema.Properties["device"].(map[string]any)["description"].(string)
	for _, want := range []string{"device_list", "host:port", "vm:NAME", "cloud://"} {
		if !strings.Contains(desc, want) {
			t.Errorf("device description %q does not mention %s", desc, want)
		}
	}
	project := runProject(t)
	for device, hint := range map[string]bool{"hopeful-glider": true, "robot.local:50051": false, "vm:test": false, "cloud://c:443/org/1/asset/2": false} {
		s := New(&config.Config{}, nil)
		s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
			return "✗ name resolver error: produced zero addresses", false, errors.New("exit status 1")
		}
		r, _ := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": project, "device": device}))
		next, _ := structuredMap(t, r)["suggested_next_step"].(string)
		if got := strings.Contains(next, fmt.Sprintf("cloud_connect(device_name=%q)", device)); got != hint {
			t.Errorf("device %q: cloud_connect hint = %v, want %v (%q)", device, got, hint, next)
		}
	}
	// A credential failure is not a resolution problem.
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		return "✗ not logged in; run 'wendy auth login' first", false, errors.New("exit status 1")
	}
	r, _ := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": project, "device": "hopeful-glider"}))
	if next, _ := structuredMap(t, r)["suggested_next_step"].(string); next != "" {
		t.Errorf("auth failure got a cloud_connect hint: %s", next)
	}
}

// runNextStep compares replay identities, not spellings: a bare host is its
// host:50051, host names ignore case, and a cloud_connect session is the same
// device as its name or its pinned selector.
func TestRunNextStepComparesNormalizedTargets(t *testing.T) {
	cloud := commandTarget{Device: "robot", Transport: "cloud", CloudGRPC: "c.example:443", Selector: "cloud://c.example:443/org/1/asset/2"}
	lan := commandTarget{Device: "robot-b.local:50051", Transport: "direct"}
	for _, tc := range []struct {
		name      string
		session   commandTarget
		args      map[string]any
		reconnect bool
	}{
		{"bare host is host:50051", lan, map[string]any{"device": "robot-b.local"}, false},
		{"host names ignore case", lan, map[string]any{"device": "Robot-B.local:50051"}, false},
		{"other LAN device", lan, map[string]any{"device": "robot-a.local"}, true},
		{"other port", lan, map[string]any{"device": "robot-b.local:50061"}, true},
		{"cloud name of the session", cloud, map[string]any{"device_name": "robot"}, false},
		{"cloud selector of the session", cloud, map[string]any{"device": "cloud://c.example:443/org/1/asset/2"}, false},
		{"same name in another cloud", cloud, map[string]any{"device_name": "robot", "cloud_grpc": "other.example:443"}, true},
		{"other cloud asset", cloud, map[string]any{"device": "cloud://c.example:443/org/1/asset/3"}, true},
		{"LAN name is not the cloud name", cloud, map[string]any{"device": "robot"}, true},
		{"simulator", commandTarget{Device: "vm:go2", Transport: "direct"}, map[string]any{"device": "vm:go2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&config.Config{}, nil)
			s.commandTarget = tc.session
			s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) { return "ok", false, nil }
			args := map[string]any{"project_path": runProject(t)}
			for k, v := range tc.args {
				args[k] = v
			}
			r, err := s.handleRun(context.Background(), callToolReq("run", args))
			if err != nil || r.IsError {
				t.Fatalf("run: %v %v", r, err)
			}
			next := structuredMap(t, r)["suggested_next_step"].(string)
			if got := strings.Contains(next, "_connect("); got != tc.reconnect {
				t.Fatalf("reconnect = %v, want %v: %s", got, tc.reconnect, next)
			}
		})
	}
}
