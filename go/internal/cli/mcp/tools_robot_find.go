package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (s *mcpServer) registerRobotFindTools(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Find a described object such as a blue chair using the paired Go2 onboard camera and an already installed vision model on the selected gateway's local Ollama runtime. Captures one fresh frame; no cloud inference and no robot motion. A visual match alone is not navigable: calibrated camera projection, matching 3D LiDAR, a stationary localized robot, and measured clear-floor approach are required. Returns seen/navigable and an opaque evidence_id when an approach is available. For a user-requested approach call robot_command action go_to_object using exactly that evidence_id. If not seen, report the current view only; this tool does not turn or explore. Requires camera approval."),
		mcpgo.WithString("serial", mcpgo.Description("Saved paired Go2 serial; omit only for exactly one paired robot.")),
		mcpgo.WithString("query", mcpgo.Required(), mcpgo.Description("One object description, including distinguishing color or shape.")),
		mcpgo.WithString("model", mcpgo.Description("Optional preferred already-installed local vision model name. The gateway chooses an installed vision-capable model if omitted.")),
	}
	opts = append(opts, mutating()...)
	opts = append(opts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("robot_find_object", opts...), s.handleRobotFindObject)
}

func (s *mcpServer) handleRobotFindObject(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	if s.robotDial == nil {
		return errResult(errCodeUnsupported, "This MCP host does not provide the digital-twin transport."), nil
	}
	query := strings.TrimSpace(stringParam(req, "query"))
	model := stringParam(req, "model")
	if query == "" || len(query) > 160 || strings.ContainsFunc(query, unicode.IsControl) || len(model) > 128 {
		return errResult(errCodeInvalidArgument, "Describe one object with 1 to 160 printable characters."), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	captureCtx, captureCancel := context.WithTimeout(ctx, 15*time.Second)
	serial, shot, _, err := s.captureGo2CalibratedSnapshot(captureCtx, stringParam(req, "serial"))
	captureCancel()
	if err != nil {
		return errResult(codeFromGRPC(err), fmt.Sprintf("Go2 camera capture failed: %v", err)), nil
	}
	result, err := requestRobotFind(ctx, serial, query, model, shot, func(ctx context.Context) (net.Conn, error) { return s.robotDial(ctx, conn) })
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	return okResult(result), nil
}

func requestRobotFind(ctx context.Context, serial, query, model string, shot cameraSnapshot, dial func(context.Context) (net.Conn, error)) (map[string]any, error) {
	if shot.timestampNS == 0 || shot.timestampNS > math.MaxInt64 {
		return nil, errors.New("Go2 camera frame has no valid gateway receipt timestamp")
	}
	data, err := json.Marshal(map[string]any{"serial": serial, "query": query, "model": model, "camera_mode": "go2-front-native-1280x720", "image": shot.jpeg, "gateway_received_unix_nanos": int64(shot.timestampNS)})
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("digital-twin redirect refused") }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://digital-twin/v1/robot/find-object", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("local object search failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			return nil, errors.New("the digital-twin app needs the local object-finding update")
		}
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("local object search: %s", bytes.TrimSpace(message))
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}
