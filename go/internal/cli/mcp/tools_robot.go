package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// SetRobotDialer reuses the CLI's authenticated digital-twin transport. No
// model-supplied host, port, shell command, or credential reaches this dialer.
func (s *mcpServer) SetRobotDialer(dial func(context.Context, *grpcclient.AgentConnection) (net.Conn, error)) {
	s.robotDial = dial
}

func (s *mcpServer) registerRobotTools(srv *server.MCPServer) {
	register := func(name, description string, read bool, props ...mcpgo.ToolOption) {
		opts := append([]mcpgo.ToolOption{mcpgo.WithDescription(description)}, props...)
		if read {
			opts = append(opts, readOnly()...)
		} else {
			opts = append(opts, mutating()...)
		}
		opts = append(opts, openWorld()...)
		srv.AddTool(mcpgo.NewTool(name, opts...), s.handleRobotTool)
	}
	register("unitree_pairings", "Read saved Unitree Go2 pairings and their current connection status directly from the selected Wendy device. Does not scan, pair, or move a robot. Use this for 'is my Go2 paired?' instead of interactive wendy device pair commands.", true)
	serial := mcpgo.WithString("serial", mcpgo.Description("Robot serial returned by unitree_pairings. Omit only when exactly one saved robot exists. Include it for Stop to avoid a pairing lookup."))
	register("robot_status", "Read the Go2's live digital-twin position, controller status, and saved named destinations. Does not acquire control or renew movement. If observations are absent, use robot_command action connect.", true, serial)
	register("robot_command", "Control the paired Go2 through its existing digital-twin controller. Actions: connect (start observations without a map click), turn (signed degrees, positive left; 1 to 180), forward (0.1 to 2 meters measured with odometry), go_to a saved destination, go_to_object with evidence from robot_find_object, remember the current map position under a name, or stop. 'Turn around' is 180 degrees. Movement is bounded to 30 seconds, uses live obstacle checks, and stops on cancellation. Reuse request_id on retries; a running receipt is not completion. Report returned state and message; never claim success for rejected/failed/superseded/canceled. Use robot_status for destination names, never invent coordinates. The user's explicit movement request authorizes that bounded goal under the current approval policy.", false, serial,
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("connect", "turn", "forward", "go_to", "go_to_object", "remember", "stop")),
		mcpgo.WithString("request_id", mcpgo.Required(), mcpgo.Description("Unique ID of 8 to 80 characters for this user command. Reuse for retries; never reuse for a different command.")),
		mcpgo.WithNumber("degrees", mcpgo.Description("For turn: integer degrees from -180 to 180, excluding zero.")),
		mcpgo.WithNumber("distance_meters", mcpgo.Min(0.1), mcpgo.Max(2), mcpgo.Description("For forward: distance in meters, 0.1 to 2.")),
		mcpgo.WithString("evidence_id", mcpgo.Description("For go_to_object: opaque evidence_id from a navigable robot_find_object result. Never invent it.")),
		mcpgo.WithString("destination", mcpgo.Description("For go_to or remember: saved map position name, such as kitchen.")))
}

func (s *mcpServer) handleRobotTool(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	serial := stringParam(req, "serial")
	// An explicit serial reaches its existing controller directly, especially
	// Stop. A slow pairing/account operation must not delay a known robot's stop.
	if req.Params.Name == "unitree_pairings" || serial == "" {
		if conn.UnitreeService == nil {
			return errResult(errCodeUnsupported, "Selected agent has no Unitree pairing service."), nil
		}
		listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		listed, err := conn.UnitreeService.ListUnitreePairings(listCtx, &pb.ListUnitreePairingsRequest{})
		cancel()
		if err != nil {
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		rows := make([]map[string]any, 0, len(listed.GetRobots()))
		for _, r := range listed.GetRobots() {
			rows = append(rows, map[string]any{"serial": r.Serial, "name": r.Name, "address": r.Address, "paired": r.Paired, "connected": r.Connected})
		}
		if req.Params.Name == "unitree_pairings" {
			return okResult(map[string]any{"robots": rows}), nil
		}
		if len(listed.GetRobots()) != 1 || !listed.Robots[0].Paired {
			return errResult(errCodeInvalidArgument, "Choose one paired robot serial from unitree_pairings."), nil
		}
		serial = listed.Robots[0].Serial
	}
	if s.robotDial == nil {
		return errResult(errCodeUnsupported, "This MCP host does not provide the digital-twin transport."), nil
	}
	args := map[string]any{"serial": serial, "action": "status"}
	if req.Params.Name == "robot_command" {
		for _, field := range []string{"action", "request_id", "degrees", "distance_meters", "destination", "evidence_id"} {
			if value, ok := req.GetArguments()[field]; ok {
				args[field] = value
			}
		}
		if args["action"] == "status" || args["action"] == "" {
			return errResult(errCodeInvalidArgument, "Specify a robot command action."), nil
		}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	ctx, done := context.WithTimeout(ctx, 40*time.Second)
	defer done()
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return s.robotDial(ctx, conn) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("digital-twin redirect refused") }}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://digital-twin/v1/robot/command", bytes.NewReader(body))
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return errResult(errCodeDeviceUnreachable, fmt.Sprintf("Digital-twin request failed; outcome is unknown. Retry with the same request_id: %v", err)), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return errResult(errCodeUnsupported, "The digital-twin app needs the chat-control update."), nil
		}
		return errResult(errCodeInvalidArgument, string(message)), nil
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&out); err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	return okResult(out), nil
}
