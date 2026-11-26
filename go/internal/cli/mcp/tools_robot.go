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
	serial := mcpgo.WithString("serial", mcpgo.Description("Serial returned by unitree_pairings. Omit when exactly one robot is paired; never invent one. Include a known serial for Stop to avoid a pairing lookup."))
	register("robot_status", "Read the Go2's live digital-twin position, controller status, and saved named destinations. ReadinessMessage and readiness fields describe current state; LastMotionFailure is historical and legacy Message may preserve it. IdleMeasurementsReady and IdleReadinessMessage permit bounded passive matching while stationary, not movement; MeasurementsReady remains strict. This only reads state, not localization. For align/localize/find my position, use robot_command action localize. If observations are absent, use action connect.", true, serial)
	register("robot_command", "Control the Go2 with bounded goals. connect starts observations. localize matches a saved room without motion; localize_active permits movement-assisted alignment only when requested. find_active uses fresh onboard camera frames and local vision to search by query: at most five minutes, six turns and three nearby viewpoints on measured clear floor; no saved object location or map required. It stops on a visual match, which does not authorize approach. These active actions use original receipt polling; never restart them. stop cancels. turn uses signed degrees; forward measures distance with odometry. go_to uses a saved destination; go_to_object needs navigable evidence_id from robot_find_object. remember saves current map position. Plain align means localize, never arbitrary turns. Individual motion goals last at most 30 seconds. Controller obstacle/freshness checks remain authoritative. Reuse request_id for retries. Report exact state/message; running is not completed. Perform supported prerequisite recovery within the turn, never repeat failed movement as recovery. find_active includes camera capture and motion under the current approval policy.", false, serial,
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("connect", "localize", "localize_active", "find_active", "turn", "forward", "go_to", "go_to_object", "remember", "stop")),
		mcpgo.WithString("request_id", mcpgo.Required(), mcpgo.Description("Unique ID of 8 to 80 characters for this user command. Reuse for retries; never reuse for a different command.")),
		mcpgo.WithBoolean("receipt_only", mcpgo.Description("Runtime retry lookup: return the existing receipt for request_id without executing or starting observations. Missing receipts have unknown outcome.")),
		mcpgo.WithNumber("degrees", mcpgo.Description("For turn: integer degrees from -180 to 180, excluding zero.")),
		mcpgo.WithNumber("distance_meters", mcpgo.Min(0.1), mcpgo.Max(2), mcpgo.Description("For forward: distance in meters, 0.1 to 2.")),
		mcpgo.WithString("query", mcpgo.Description("For find_active: one printable object description, 1 to 160 characters.")),
		mcpgo.WithString("model", mcpgo.Description("For find_active: optional installed local vision model; omit for automatic selection.")),
		mcpgo.WithString("evidence_id", mcpgo.Description("For go_to_object: opaque evidence_id from a navigable robot_find_object result. Never invent it.")),
		mcpgo.WithString("destination", mcpgo.Description("For go_to or remember: saved map position name. For localize or localize_active: optional saved room name; omit only when the room can be selected unambiguously.")))
}

func (s *mcpServer) handleRobotTool(ctx context.Context, req mcpgo.CallToolRequest) (result *mcpgo.CallToolResult, resultErr error) {
	dispatched := false
	resolvedSerial := ""
	defer func() {
		// Bind transport retries and Stop acknowledgement to the robot chosen
		// by this MCP connection, without making the model carry its identity.
		if result != nil && resolvedSerial != "" {
			if result.Meta == nil {
				result.Meta = &mcpgo.Meta{}
			}
			if result.Meta.AdditionalFields == nil {
				result.Meta.AdditionalFields = make(map[string]any)
			}
			result.Meta.AdditionalFields["robot_serial"] = resolvedSerial
		}
		if req.Params.Name == "robot_command" && !dispatched && result != nil && result.IsError {
			if fields, ok := result.StructuredContent.(map[string]any); ok {
				fields["command_not_started"] = true
			}
		}
	}()
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
	resolvedSerial = serial
	if s.robotDial == nil {
		return errResult(errCodeUnsupported, "This MCP host does not provide the digital-twin transport."), nil
	}
	args := map[string]any{"serial": serial, "action": "status"}
	if req.Params.Name == "robot_command" {
		for _, field := range []string{"action", "request_id", "degrees", "distance_meters", "destination", "evidence_id", "query", "model"} {
			if value, ok := req.GetArguments()[field]; ok {
				args[field] = value
			}
		}
		if args["action"] == "status" || args["action"] == "" {
			return errResult(errCodeInvalidArgument, "Specify a robot command action."), nil
		}
		if lookup, _ := req.GetArguments()["receipt_only"].(bool); lookup {
			args["action"] = "receipt"
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
	dispatched = true
	resp, err := client.Do(httpReq)
	if err != nil {
		return errResult(errCodeDeviceUnreachable, fmt.Sprintf("Digital-twin request failed; outcome is unknown. Retry with the same request_id: %v", err)), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// This fixed command endpoint returns 400 only before controller
		// dispatch; controller outcomes are 200 receipts. A missing route also
		// cannot have dispatched. Other failures remain ambiguous.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
			dispatched = false
		}
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
	if req.Params.Name == "robot_command" && stringParam(req, "action") == "find_active" {
		out, err = provideRobotSearchView(ctx, serial, stringParam(req, "request_id"), out,
			func(ctx context.Context, serial string) (string, cameraSnapshot, int64, error) {
				return captureGo2SnapshotOnConn(ctx, conn, serial, true)
			}, func(ctx context.Context) (net.Conn, error) { return s.robotDial(ctx, conn) })
		if err != nil {
			return errResult(errCodeDeviceUnreachable, "Object-search camera handshake failed; use the same receipt to check its outcome, do not restart motion: "+err.Error()), nil
		}
	}
	return okResult(out), nil
}
