package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func workspaceGateway(t *testing.T) (*RobotGateway, context.Context, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := gatewayTestConfig()
	cfg.StateDirectory = t.TempDir()
	cfg.AllowSimulators = true
	cfg.Workspaces = []GatewayWorkspace{{ID: "project", Name: "Project", Path: dir, Robots: []string{"alpha"}, AllowSimulators: true}}
	cfg.Grants[0].Workspaces = []string{"project"}
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("offline")
	}, WithGatewayLifecycle(onboarding.Backend{}, ProjectBackend{Validate: func(context.Context, ProjectValidationOptions) (*ProjectValidation, error) {
		return &ProjectValidation{Valid: true}, nil
	}}, SimulatorBackend{List: func(context.Context) ([]SimulatorInfo, error) {
		return []SimulatorInfo{{Name: "dev", Device: "vm:dev", State: "running", Profile: "go2"}, {Name: "stopped", Device: "vm:stopped", State: "stopped"}}, nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return g, gatewayAccessContext(robotGatewayScopes, true), dir
}

func workspaceCall(t *testing.T, g *RobotGateway, ctx context.Context, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	result, err := g.protocol.ListTools()[name].Handler(ctx, callToolReq(name, args))
	if err != nil || result == nil {
		t.Fatalf("%s: %v, %v", name, result, err)
	}
	return result
}

func TestGatewayWorkspaceEditRevisionAndContainment(t *testing.T) {
	g, ctx, dir := workspaceGateway(t)
	write := func(path, content, revision string) *mcpgo.CallToolResult {
		return workspaceCall(t, g, ctx, "write_workspace_file", map[string]any{"workspace_id": "project", "path": path, "content": content, "expected_sha256": revision})
	}
	if result := write("src/main.py", "print('first')\n", "missing"); result.IsError {
		t.Fatal(result)
	}
	r := workspaceCall(t, g, ctx, "read_workspace_file", map[string]any{"workspace_id": "project", "path": "src/main.py"})
	data := r.StructuredContent.(map[string]any)
	if r.IsError || data["content"] != "print('first')\n" {
		t.Fatal(r)
	}
	if result := write("src/main.py", "print('second')\n", data["sha256"].(string)); result.IsError {
		t.Fatal(result)
	}
	for _, revision := range []string{"missing", data["sha256"].(string), ""} {
		if !write("src/main.py", "stale", revision).IsError {
			t.Fatal("stale edit replaced current content")
		}
	}
	current, _ := os.ReadFile(filepath.Join(dir, "src/main.py"))
	if string(current) != "print('second')\n" {
		t.Fatal(string(current))
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "/etc/passwd", "src/../../secret", "escape/secret", "escape/new", ".git/config", ".GiT/config", "alias/main.py", "alias/new", "src\\escape"} {
		if !write(path, "changed", "missing").IsError {
			t.Fatalf("write escaped workspace via %q", path)
		}
		r := workspaceCall(t, g, ctx, "read_workspace_file", map[string]any{"workspace_id": "project", "path": path})
		if !r.IsError {
			t.Fatalf("read escaped workspace via %q", path)
		}
	}
	if !write("large", strings.Repeat("x", gatewayWorkspaceFileLimit+1), "missing").IsError || !write("binary", "\x00", "missing").IsError {
		t.Fatal("unbounded or binary write accepted")
	}
	list := workspaceCall(t, g, ctx, "list_workspace_files", map[string]any{"workspace_id": "project", "path": "src"})
	if list.IsError || len(list.StructuredContent.(map[string]any)["entries"].([]map[string]any)) != 1 {
		t.Fatal(list)
	}
	secret, _ := os.ReadFile(filepath.Join(outside, "secret"))
	if string(secret) != "private" {
		t.Fatal("external file was modified")
	}
}

func TestGatewayWorkspaceFilePermissions(t *testing.T) {
	for _, tc := range []struct {
		name, missingScope string
		local, granted     bool
	}{
		{"HTTP", "", false, true},
		{"no workspace grant", "", true, false},
		{"no read scope", RobotProjectScope, true, true},
		{"no write scope", RobotProjectWriteScope, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, dir := workspaceGateway(t)
			if !tc.granted {
				g.cfg.Grants[0].Workspaces = nil
			}
			ctx := gatewayAccessContext(gatewayAccessScopesWithout(tc.missingScope), tc.local)
			for _, name := range gatewayWorkspaceFileTools {
				denied := !tc.local || !tc.granted || g.toolScope(name) == tc.missingScope
				r := workspaceCall(t, g, ctx, name, map[string]any{"workspace_id": "project", "path": "new", "content": "code", "expected_sha256": "missing"})
				if denied && !r.IsError {
					t.Fatalf("%s bypassed policy", name)
				}
			}
			if !tc.local || !tc.granted || tc.missingScope == RobotProjectWriteScope {
				if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
					t.Fatal("denied write changed workspace")
				}
			}
			var tools []mcpgo.Tool
			for _, tool := range g.protocol.ListTools() {
				tools = append(tools, tool.Tool)
			}
			for _, tool := range g.filterTools(ctx, tools) {
				if slices.Contains(gatewayWorkspaceFileTools, tool.Name) && (!tc.local || g.toolScope(tool.Name) == tc.missingScope) {
					t.Fatalf("unavailable tool advertised: %s", tool.Name)
				}
			}
		})
	}
}

func TestGatewayWorkspaceDirectoryPagination(t *testing.T) {
	g, ctx, dir := workspaceGateway(t)
	for n := 0; n < 205; n++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d", n)), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"workspace_id": "project"}
	first := workspaceCall(t, g, ctx, "list_workspace_files", args).StructuredContent.(map[string]any)
	rows := first["entries"].([]map[string]any)
	if len(rows) != 200 || rows[0]["path"] != "file-000" || first["next_offset"] != 200 {
		t.Fatal(first)
	}
	args["offset"] = first["next_offset"]
	last := workspaceCall(t, g, ctx, "list_workspace_files", args).StructuredContent.(map[string]any)
	rows = last["entries"].([]map[string]any)
	if len(rows) != 5 || rows[0]["path"] != "file-200" || last["next_offset"] != nil {
		t.Fatal(last)
	}
}

func TestGatewaySimulatorProjectDeploymentLoop(t *testing.T) {
	g, ctx, dir := workspaceGateway(t)
	created := workspaceCall(t, g, ctx, "write_workspace_file", map[string]any{"workspace_id": "project", "path": "main.py", "content": "print('hello simulator')", "expected_sha256": "missing"})
	if created.IsError {
		t.Fatal(created)
	}
	listed := workspaceCall(t, g, ctx, "list_workspaces", nil).StructuredContent.(map[string]any)["workspaces"].([]map[string]any)
	if !slices.Contains(listed[0]["robot_ids"].([]string), "sim-dev") || listed[0]["can_write_files"] != true {
		t.Fatal(listed)
	}
	var runs atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	t.Cleanup(func() { close(finish) })
	g.lifecycle.runCommandFn = func(ctx context.Context, args []string, target commandTarget, limit int) (string, bool, error) {
		runs.Add(1)
		if target.Device != "vm:dev" || !slices.Contains(args, dir) {
			t.Errorf("deployment target changed: %v, %v", target, args)
		}
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return "started", false, nil
	}
	args := map[string]any{"workspace_id": "project", "robot_id": "sim-dev", "request_id": "test-build"}
	job := workspaceCall(t, g, ctx, "start_device_deployment", args)
	if job.IsError {
		t.Fatal(job)
	}
	id := job.StructuredContent.(gatewayDeployment).ID
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("deployment never reached run backend")
	}
	again := workspaceCall(t, g, ctx, "start_device_deployment", args)
	if again.IsError || again.StructuredContent.(gatewayDeployment).ID != id || runs.Load() != 1 {
		t.Fatal("uncertain response duplicated deployment", again)
	}
	cancel := workspaceCall(t, g, ctx, "cancel_deployment_job", map[string]any{"job_id": id})
	if cancel.IsError {
		t.Fatal(cancel)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result := workspaceCall(t, g, ctx, "get_deployment_job", map[string]any{"job_id": id})
		j := result.StructuredContent.(gatewayDeployment)
		if j.Status == "interrupted" {
			if j.Readiness != "unknown" {
				t.Fatal("deployment claimed readiness")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled deployment did not retain an interrupted result")
}

func TestGatewaySimulatorDeploymentPolicy(t *testing.T) {
	for _, name := range []string{"HTTP", "workspace opt-in", "simulators disabled", "no deploy scope", "explicit policy", "explicit workspace binding", "stopped simulator"} {
		t.Run(name, func(t *testing.T) {
			g, ctx, _ := workspaceGateway(t)
			id := "sim-dev"
			switch name {
			case "HTTP":
				ctx = gatewayAccessContext(robotGatewayScopes, false)
			case "workspace opt-in":
				g.cfg.Workspaces[0].AllowSimulators = false
			case "simulators disabled":
				g.cfg.AllowSimulators = false
			case "no deploy scope":
				ctx = gatewayAccessContext(gatewayAccessScopesWithout(RobotDeployScope), true)
			case "explicit policy":
				g.cfg.Robots[1].ID, g.cfg.Robots[1].Device = "sim-dev", "vm:dev"
			case "explicit workspace binding":
				g.cfg.Robots[1].ID, g.cfg.Robots[1].Device = "sim-dev", "vm:dev"
				g.cfg.Grants[0].Robots = append(g.cfg.Grants[0].Robots, "sim-dev")
			case "stopped simulator":
				id = "sim-stopped"
			}
			g.lifecycle.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
				t.Error("denied deployment reached command runner")
				return "", false, nil
			}
			r := workspaceCall(t, g, ctx, "start_device_deployment", map[string]any{"workspace_id": "project", "robot_id": id, "request_id": "denied"})
			if !r.IsError {
				t.Fatal("denied target was deployed", r)
			}
		})
	}
}

func TestGatewaySimulatorDeploymentCompletedJobSurvivesRestart(t *testing.T) {
	g, ctx, _ := workspaceGateway(t)
	var runs atomic.Int32
	g.lifecycle.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		runs.Add(1)
		return "application started", false, nil
	}
	args := map[string]any{"workspace_id": "project", "robot_id": "sim-dev", "request_id": "completed"}
	job := workspaceCall(t, g, ctx, "start_device_deployment", args)
	if job.IsError {
		t.Fatal(job)
	}
	id := job.StructuredContent.(gatewayDeployment).ID
	deadline := time.Now().Add(3 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		result := workspaceCall(t, g, ctx, "get_deployment_job", map[string]any{"job_id": id})
		j := result.StructuredContent.(gatewayDeployment)
		if j.Status == "completed" {
			if j.Readiness != "unknown" || j.Result == nil || j.Result.IsError {
				t.Fatal("invalid completed result", j)
			}
			completed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !completed {
		t.Fatal("deployment did not complete")
	}
	restarted, err := NewRobotGateway(g.cfg, g.connect, WithGatewayLifecycle(onboarding.Backend{}, g.lifecycle.project, g.lifecycle.simulators))
	if err != nil {
		t.Fatal(err)
	}
	restarted.lifecycle.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		runs.Add(1)
		return "unexpected retry", false, nil
	}
	again := workspaceCall(t, restarted, ctx, "start_device_deployment", args)
	j := again.StructuredContent.(gatewayDeployment)
	if again.IsError || j.ID != id || j.Status != "completed" || runs.Load() != 1 {
		t.Fatal("restart lost or repeated completed deployment", again)
	}
}

func TestGatewaySimulatorOnlyConfig(t *testing.T) {
	cfg := RobotGatewayConfig{AllowSimulators: true, LocalSubject: "owner", Grants: []GatewayGrant{{Subject: "owner", Scopes: []string{RobotReadScope, RobotSimulatorScope}}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.AllowSimulators = false
	if err := cfg.validate(); err == nil {
		t.Fatal("empty device policy accepted without simulators")
	}
}
