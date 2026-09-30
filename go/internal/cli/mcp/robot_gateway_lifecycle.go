package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type gatewayLocalContextKey struct{}

var gatewayHostTools = []string{"os_install_plan", "os_list_drives", "os_install_start", "os_install_status", "os_install_resume", "os_install_verify", "simulator_list", "simulator_create", "simulator_start", "simulator_update_agent", "simulator_viewer", "simulator_stop", "simulator_delete"}
var gatewayLifecycleTools = []string{"list_workspaces", "validate_device_project", "plan_fleet_deployment", "start_device_deployment", "get_deployment_job", "cancel_deployment_job"}

func WithGatewayLifecycle(install onboarding.Backend, project ProjectBackend, simulator SimulatorBackend) RobotGatewayOption {
	return func(g *RobotGateway) {
		s := New(&config.Config{}, g.connect)
		s.SetInstallationBackend(install)
		s.SetProjectBackend(project)
		s.SetSimulatorBackend(simulator)
		g.lifecycle = s
	}
}
func (g *RobotGateway) localHostAllowed(ctx context.Context) bool {
	_, p := g.grant(ctx)
	return ctx.Value(gatewayLocalContextKey{}) == true && g.cfg.AllowHostOperations && p.Subject == g.cfg.LocalSubject && g.hasScope(ctx, RobotHostScope)
}

func (g *RobotGateway) localSimulatorAllowed(ctx context.Context) bool {
	_, p := g.grant(ctx)
	return g.localHostAllowed(ctx) || (ctx.Value(gatewayLocalContextKey{}) == true && g.cfg.AllowSimulators && p.Subject == g.cfg.LocalSubject && g.hasScope(ctx, RobotSimulatorScope))
}
func (g *RobotGateway) workspace(ctx context.Context, id, robot string) (GatewayWorkspace, error) {
	grant, _ := g.grant(ctx)
	if grant == nil || !slices.Contains(grant.Workspaces, id) {
		return GatewayWorkspace{}, fmt.Errorf("workspace is not authorized")
	}
	for _, w := range g.cfg.Workspaces {
		if w.ID == id && (robot == "" || slices.Contains(w.Robots, robot)) {
			return w, nil
		}
	}
	return GatewayWorkspace{}, fmt.Errorf("workspace is not permitted on this device")
}
func (g *RobotGateway) registerLifecycleTools() {
	if g.lifecycle == nil {
		g.lifecycle = New(&config.Config{}, g.connect)
	}
	g.jobCancels = map[string]context.CancelFunc{}
	host := server.NewMCPServer("wendy-host", "1")
	g.lifecycle.registerInstallationTools(host)
	g.lifecycle.registerSimulatorTools(host)
	for _, t := range host.ListTools() {
		if t.Tool.Name == "simulator_create" {
			mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional)(&t.Tool)
		}
		g.protocol.AddTool(t.Tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if req.Params.Task != nil {
				var err error
				ctx, err = gatewayTaskContext(req)
				if err != nil {
					return mcpgo.NewToolResultError(err.Error()), nil
				}
			}
			allowed := g.localHostAllowed(ctx)
			if strings.HasPrefix(req.Params.Name, "simulator_") {
				allowed = g.localSimulatorAllowed(ctx)
			}
			if !allowed {
				return mcpgo.NewToolResultError("Host operations require an explicitly enabled local stdio session."), nil
			}
			result, err := t.Handler(ctx, req)
			if req.Params.Name == "simulator_create" && err == nil && !result.IsError {
				if data, ok := result.StructuredContent.(map[string]any); ok {
					data["next_step"] = "simulator_start"
					return okResult(data), nil
				}
			}
			return result, err
		})
	}
	g.registerGatewaySimulators()
	ws := mcpgo.WithString("workspace_id", mcpgo.Required(), mcpgo.MaxLength(64))
	g.protocol.AddTool(gatewayTool("list_workspaces", "List approved local project workspaces and their permitted device IDs. A browser cannot access arbitrary host files.", readOnly()), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !g.hasScope(ctx, RobotProjectScope) {
			return mcpgo.NewToolResultError("Unauthorized"), nil
		}
		out := []map[string]any{}
		grant, _ := g.grant(ctx)
		for _, w := range g.cfg.Workspaces {
			if _, err := g.workspace(ctx, w.ID, ""); err == nil {
				ids := []string{}
				for _, id := range w.Robots {
					if slices.Contains(grant.Robots, id) {
						ids = append(ids, id)
					}
				}
				out = append(out, map[string]any{"id": w.ID, "name": w.Name, "robot_ids": ids})
			}
		}
		return okResult(map[string]any{"workspaces": out}), nil
	})
	g.protocol.AddTool(gatewayTool("validate_device_project", "Validate an approved project against an explicitly selected device before deployment. Does not build or run hooks.", readOnly(), ws, robotArgument()), g.withRobot(RobotProjectScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		w, err := g.workspace(ctx, req.GetString("workspace_id", ""), r.ID)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		s.SetProjectBackend(g.lifecycle.project)
		q := mcpgo.CallToolRequest{}
		q.Params.Arguments = map[string]any{"project_path": w.Path}
		return s.handleProjectValidate(ctx, q)
	}))
	g.protocol.AddTool(gatewayTool("plan_fleet_deployment", "Freeze an explicit set of authorized targets for a project. Deploy one canary with start_device_deployment, inspect its app and readiness, then explicitly deploy subsequent targets. No automatic fleet fan-out or rollback.", readOnly(), ws, mcpgo.WithArray("robot_ids", mcpgo.Required(), mcpgo.Items(map[string]any{"type": "string"}))), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !g.hasScope(ctx, RobotProjectScope) {
			return mcpgo.NewToolResultError("Unauthorized"), nil
		}
		ids := req.GetStringSlice("robot_ids", nil)
		if len(ids) == 0 || len(ids) > 100 {
			return mcpgo.NewToolResultError("Select 1..100 explicit devices"), nil
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				return mcpgo.NewToolResultError("Repeated target"), nil
			}
			seen[id] = true
			if _, err := g.authorize(ctx, id, RobotProjectScope); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if _, err := g.workspace(ctx, req.GetString("workspace_id", ""), id); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}
		return okResult(map[string]any{"workspace_id": req.GetString("workspace_id", ""), "robot_ids": ids, "canary": ids[0], "status": "planned", "readiness_gate": "Verify the canary's actual app or ROS output before deploying further targets."}), nil
	})
	g.protocol.AddTool(gatewayTool("start_device_deployment", "Build and deploy an approved project to one explicit device. Validates first. Returns a durable job ID; poll get_deployment_job. Reuse the same request_id after an uncertain response to avoid duplicate deployment. Executes approved project build hooks; success does not prove application readiness.", destructive(), ws, robotArgument(), mcpgo.WithString("request_id", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(64))), g.startGatewayDeployment)
	for _, name := range []string{"get_deployment_job", "cancel_deployment_job"} {
		behavior := readOnly()
		if name == "cancel_deployment_job" {
			behavior = mutating()
		}
		g.protocol.AddTool(gatewayTool(name, "Read or cancel your deployment job. Cancellation may leave an already deployed app running; inspect before retrying.", behavior, mcpgo.WithString("job_id", mcpgo.Required(), mcpgo.MaxLength(64))), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if !g.hasScope(ctx, RobotDeployScope) {
				return mcpgo.NewToolResultError("Unauthorized"), nil
			}
			id := req.GetString("job_id", "")
			if !gatewayIdentifier.MatchString(id) {
				return mcpgo.NewToolResultError("Invalid job ID"), nil
			}
			g.jobMu.Lock()
			defer g.jobMu.Unlock()
			j, err := g.readDeployment(ctx, id)
			if err != nil {
				return mcpgo.NewToolResultError("Job unavailable for this account"), nil
			}
			if _, err := g.authorize(ctx, j.RobotID, RobotDeployScope); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if req.Params.Name == "cancel_deployment_job" && g.jobCancels[id] != nil {
				j.Status = "cancelling"
				if err := g.saveDeployment(ctx, j); err != nil {
					return nil, err
				}
				g.jobCancels[id]()
			}
			if j.Status == "running" && g.jobCancels[id] == nil {
				j.Status = "interrupted"
				j.Note = "Gateway restarted. Inspect the device before retrying."
			}
			return okResult(j), nil
		})
	}
}

type gatewayDeployment struct {
	ID          string                `json:"id"`
	WorkspaceID string                `json:"workspace_id"`
	RobotID     string                `json:"robot_id"`
	Status      string                `json:"status"`
	StartedAt   string                `json:"started_at"`
	Readiness   string                `json:"readiness"`
	Note        string                `json:"note,omitempty"`
	Result      *mcpgo.CallToolResult `json:"result,omitempty"`
}

func (g *RobotGateway) deploymentPath(ctx context.Context, id string) (string, error) {
	p, err := g.settingsPath(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), filepath.Base(p)+"-jobs", id+".json"), nil
}
func (g *RobotGateway) readDeployment(ctx context.Context, id string) (gatewayDeployment, error) {
	var j gatewayDeployment
	p, err := g.deploymentPath(ctx, id)
	if err != nil {
		return j, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return j, err
	}
	err = json.Unmarshal(b, &j)
	return j, err
}
func (g *RobotGateway) saveDeployment(ctx context.Context, j gatewayDeployment) error {
	p, err := g.deploymentPath(ctx, j.ID)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return atomicfile.Write(p, b, 0600)
}
func (g *RobotGateway) startGatewayDeployment(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	r, err := g.authorize(ctx, req.GetString("robot_id", ""), RobotDeployScope)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	w, err := g.workspace(ctx, req.GetString("workspace_id", ""), r.ID)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	requestID := req.GetString("request_id", "")
	if !gatewayIdentifier.MatchString(requestID) {
		return mcpgo.NewToolResultError("request_id must be a stable identifier"), nil
	}
	_, p := g.grant(ctx)
	hash := sha256.Sum256([]byte(p.Subject + "\x00" + requestID))
	id := fmt.Sprintf("deploy-%x", hash[:16])
	g.jobMu.Lock()
	defer g.jobMu.Unlock()
	if j, err := g.readDeployment(ctx, id); err == nil {
		if j.WorkspaceID != w.ID || j.RobotID != r.ID {
			return mcpgo.NewToolResultError("request_id already belongs to another deployment"), nil
		}
		return okResult(j), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if len(g.jobCancels) >= 4 {
		return mcpgo.NewToolResultError("Four deployment jobs are already active"), nil
	}
	if g.lifecycle.project.Validate == nil {
		return mcpgo.NewToolResultError("Project backend unavailable"), nil
	}
	// Fail before a build if local inputs are invalid. Device-specific checks are
	// available through validate_device_project; unknown compatibility stays unknown.
	v, err := g.lifecycle.project.Validate(ctx, ProjectValidationOptions{ProjectPath: w.Path})
	if err != nil {
		return mcpgo.NewToolResultError("Project validation failed"), nil
	}
	if !v.Valid {
		return okResult(map[string]any{"status": "validation_failed", "validation": v}), nil
	}
	path, err := g.deploymentPath(ctx, id)
	if err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) >= 256 {
		return mcpgo.NewToolResultError("Deployment history is full. Archive completed jobs on the gateway host."), nil
	}
	j := gatewayDeployment{ID: id, WorkspaceID: w.ID, RobotID: r.ID, Status: "running", StartedAt: time.Now().UTC().Format(time.RFC3339), Readiness: "unknown"}
	if err = g.saveDeployment(ctx, j); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	g.jobCancels[id] = cancel
	go func() {
		defer cancel()
		q := mcpgo.CallToolRequest{}
		q.Params.Arguments = map[string]any{"project_path": w.Path, "device": r.Device, "timeout_seconds": 1800, "max_bytes": 24000}
		result, err := g.lifecycle.handleRun(runCtx, q)
		g.jobMu.Lock()
		defer g.jobMu.Unlock()
		defer delete(g.jobCancels, id)
		j.Result = result
		j.Status = "completed"
		if err != nil || (result != nil && result.IsError) {
			j.Status = "failed"
		}
		if runCtx.Err() != nil {
			j.Status = "interrupted"
		}
		j.Note = "Inspect the container, logs, and actual app or ROS output before treating the app as ready."
		if e := g.saveDeployment(ctx, j); e != nil {
			fmt.Fprintln(os.Stderr, "Could not persist deployment result:", e)
		}
	}()
	return okResult(j), nil
}
