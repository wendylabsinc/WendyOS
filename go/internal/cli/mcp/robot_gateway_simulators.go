package mcp

import (
	"context"
	"fmt"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"time"
)

func (g *RobotGateway) discoveredSimulatorAllowed(ctx context.Context, id string) bool {
	if !g.localSimulatorAllowed(ctx) {
		return false
	}
	// A configured simulator keeps its explicit workspace and subject policy.
	for _, robot := range g.cfg.Robots {
		if robot.ID == id {
			return false
		}
	}
	rows, _ := g.catalog(ctx, false)
	for _, row := range rows {
		if row.ID == id && row.source == "simulator" {
			return true
		}
	}
	return false
}

func (g *RobotGateway) registerGatewaySimulators() {
	for _, name := range []string{"simulator_start", "simulator_update_agent", "simulator_viewer"} {
		opts := []mcpgo.ToolOption{mcpgo.WithString("name", mcpgo.Required(), mcpgo.MaxLength(32))}
		behavior := readOnly()
		description := "Read the verified live robot simulation URL for this local simulator. Does not start or move it. Open the URL to visualize its actual state."
		if name == "simulator_viewer" {
			opts = append(opts, mcpgo.WithBoolean("embedded", mcpgo.DefaultBool(false), mcpgo.Description("Create a temporary read-only scene session for rendering inside the Wendy UI. Access details are returned only in UI metadata.")))
		}
		if name == "simulator_start" {
			behavior = mutating()
			opts = append(opts, mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional))
			description = "Boot an existing laptop simulator and provision its robot runtime. First start can take several minutes. Use task augmentation when supported. This only targets vm:name, never physical hardware. Read simulator_viewer after completion to open its live robot scene."
		}
		if name == "simulator_update_agent" {
			behavior = mutating()
			opts = append(opts, mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional))
			description = "Finish setup of an existing running laptop robot simulator by installing the official stable Wendy agent only if its robot capability is missing. Restarts only that VM's agent, verifies its binary and robot capability, and leaves applications and the VM disk in place. Requires a named local VM, never physical hardware. After completion call simulator_start to provision the robot runtime."
		}
		g.protocol.AddTool(gatewayTool(name, description, behavior, opts...), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if req.Params.Task != nil {
				var err error
				ctx, err = gatewayTaskContext(req)
				if err != nil {
					return mcpgo.NewToolResultError(err.Error()), nil
				}
			}
			if !g.localSimulatorAllowed(ctx) {
				return mcpgo.NewToolResultError("Laptop simulators require local host access in the gateway policy."), nil
			}
			name := req.GetString("name", "")
			if err := vm.ValidName(name); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if req.Params.Name == "simulator_update_agent" {
				if g.lifecycle.simulators.UpdateAgent == nil {
					return mcpgo.NewToolResultError("Simulator agent updates are unavailable in this gateway build."), nil
				}
				ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				result, err := g.lifecycle.simulators.UpdateAgent(ctx, name)
				if err != nil {
					return mcpgo.NewToolResultError("Simulator setup was not confirmed: " + err.Error() + ". Refresh its status before retrying."), nil
				}
				return okResult(result), nil
			}
			if req.Params.Name == "simulator_start" {
				ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
				defer cancel()
				if g.lifecycle.simulators.Start != nil {
					info, err := g.lifecycle.simulators.Start(ctx, name)
					if err != nil {
						return simulatorFailure(err), nil
					}
					next := "simulator_viewer"
					if vm.IsLiteProfile(info.Profile) {
						next = "wendy run --device vm:" + name
					}
					return okResult(map[string]any{"name": name, "device": info.Device, "profile": info.Profile, "readiness": "verified", "next_step": next}), nil
				}
				conn, err := g.connect(ctx, "vm:"+name)
				if err != nil {
					return mcpgo.NewToolResultError("Simulator start was not confirmed: " + err.Error() + ". Refresh its status before retrying."), nil
				}
				defer conn.Close()
				return okResult(map[string]any{"name": name, "device": "vm:" + name, "agent_connected": true, "next_step": "simulator_viewer"}), nil
			}
			if g.lifecycle.simulators.Viewer == nil {
				return mcpgo.NewToolResultError("Simulator viewer is unavailable in this gateway build."), nil
			}
			viewer, err := g.lifecycle.simulators.Viewer(ctx, name)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("Simulator viewer is not ready: %v", err)), nil
			}
			result := okResult(viewer)
			if req.GetBool("embedded", false) && viewer.Ready && viewer.Healthy {
				session, err := g.openSimulatorScene(ctx, viewer, 30*time.Minute)
				if err != nil {
					result.Meta = mcpgo.NewMetaFromMap(map[string]any{"scene_session_error": err.Error()})
				} else {
					result.Meta = mcpgo.NewMetaFromMap(map[string]any{"scene_session": session})
				}
			}
			return result, nil
		})
	}
}
