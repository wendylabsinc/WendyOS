package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Camera tools. The agent owns /dev/videoN -- StreamVideo multiplexes one
// capture to every subscriber -- so it is the only writer whose control changes
// survive a pipeline reopen. That makes these worth exposing to an agent: the
// loop they enable is "look at the picture, notice the exposure is wrong, fix
// it, put it back", and every step of it needs the device.
//
// Only local (USB/CSI) cameras have V4L2 controls; a network camera has none.
func (s *mcpServer) registerCameraTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List the cameras attached to the connected device, with the id each control tool takes"),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("camera_list", listOpts...), s.handleCameraList)

	ctrlOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Show a local camera's tunable controls (exposure, gain, zoom, focus, ...) with the current value, range and driver default. The list comes from the camera, so it is whatever that hardware supports."),
		mcpgo.WithNumber("device_id",
			mcpgo.Description("Camera id from camera_list (the N in /dev/videoN)"),
			mcpgo.Required(),
		),
	}
	ctrlOpts = append(ctrlOpts, readOnly()...)
	ctrlOpts = append(ctrlOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("camera_controls", ctrlOpts...), s.handleCameraControls)

	setOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Set tunable controls on a local camera. Values persist across stream reconnects and reboots unless persist=false. Call camera_controls first for the names and ranges this camera accepts."),
		mcpgo.WithNumber("device_id",
			mcpgo.Description("Camera id from camera_list"),
			mcpgo.Required(),
		),
		mcpgo.WithString("controls",
			mcpgo.Description(`Comma-separated name=value pairs, e.g. "auto_exposure=1,exposure_time_absolute=20". A mode control is applied before the control it gates.`),
		),
		mcpgo.WithString("reset",
			mcpgo.Description("Comma-separated control names to put back to the driver's default and stop persisting"),
		),
		mcpgo.WithBoolean("reset_all",
			mcpgo.Description("Put every control back to the driver's default and forget them all"),
		),
		mcpgo.WithBoolean("persist",
			mcpgo.Description("Re-apply on stream reconnect and reboot (default true)"),
		),
	}
	// Mutating rather than destructive: it changes how the camera captures and
	// that is durable, but nothing is removed and reset/reset_all undo it.
	// Idempotent -- setting the same value twice lands the same camera state.
	setOpts = append(setOpts, mutating()...)
	setOpts = append(setOpts, idempotent()...)
	setOpts = append(setOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("camera_set_control", setOpts...), s.handleCameraSetControl)
}

func (s *mcpServer) handleCameraList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	cams := make([]map[string]any, 0, len(resp.GetDevices()))
	for _, d := range resp.GetDevices() {
		cams = append(cams, map[string]any{
			"device_id": d.GetId(),
			"name":      d.GetName(),
			"path":      d.GetPath(),
			"driver":    d.GetDriver(),
			"online":    d.GetOnline(),
		})
	}
	return okResultBounded(map[string]any{"cameras": cams}, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleCameraControls(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	id, err := cameraDeviceID(req)
	if err != nil {
		return errResultf(errCodeInvalidArgument, "%v", err), nil
	}
	resp, err := conn.VideoService.GetCameraControls(ctx,
		&agentpb.GetCameraControlsRequest{DeviceId: id})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	out := make([]map[string]any, 0, len(resp.GetControls()))
	for _, c := range resp.GetControls() {
		out = append(out, map[string]any{
			"name":    c.GetName(),
			"value":   c.GetValue(),
			"minimum": c.GetMinimum(),
			"maximum": c.GetMaximum(),
			"step":    c.GetStep(),
			"default": c.GetDefaultValue(),
			"mutable": c.GetMutable(),
		})
	}
	return okResultBounded(map[string]any{"device_id": id, "controls": out},
		intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleCameraSetControl(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	id, err := cameraDeviceID(req)
	if err != nil {
		return errResultf(errCodeInvalidArgument, "%v", err), nil
	}

	var controls []*agentpb.CameraControlSetting
	for _, pair := range splitList(stringParam(req, "controls")) {
		name, val, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return errResultf(errCodeInvalidArgument, "expected name=value, got %q", pair), nil
		}
		n, convErr := strconv.ParseInt(strings.TrimSpace(val), 10, 32)
		if convErr != nil {
			return errResultf(errCodeInvalidArgument, "control %q: value %q is not an integer", name, val), nil
		}
		controls = append(controls, &agentpb.CameraControlSetting{Name: name, Value: int32(n)})
	}

	resetNames := splitList(stringParam(req, "reset"))
	if req.GetBool("reset_all", false) {
		// Ask the camera what it has rather than keeping a list here, so this
		// covers whatever the attached hardware exposes -- and every control,
		// not only the currently mutable ones: a control gated inactive by a
		// mode is exactly the one that would otherwise stay pinned.
		got, listErr := conn.VideoService.GetCameraControls(ctx,
			&agentpb.GetCameraControlsRequest{DeviceId: id})
		if listErr != nil {
			return errResult(codeFromGRPC(listErr), grpcErrString(listErr)), nil
		}
		for _, c := range got.GetControls() {
			resetNames = append(resetNames, c.GetName())
		}
	}
	if len(controls) == 0 && len(resetNames) == 0 {
		return errResultf(errCodeInvalidArgument,
			"nothing to do: give controls, reset, or reset_all"), nil
	}

	// Setting and resetting are separate RPCs: reset changes what is PERSISTED,
	// not just the value. Both may appear in one call, so both are sent and the
	// results reported as one list.
	var allResults []*agentpb.CameraControlResult
	if len(controls) > 0 {
		resp, err := conn.VideoService.SetCameraControls(ctx, &agentpb.SetCameraControlsRequest{
			DeviceId: id,
			Controls: controls,
			Persist:  req.GetBool("persist", true),
		})
		if err != nil {
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		allResults = append(allResults, resp.GetResults()...)
	}
	if len(resetNames) > 0 {
		resp, err := conn.VideoService.ResetCameraControls(ctx, &agentpb.ResetCameraControlsRequest{
			DeviceId: id,
			Names:    resetNames,
		})
		if err != nil {
			return errResult(codeFromGRPC(err), grpcErrString(err)), nil
		}
		allResults = append(allResults, resp.GetResults()...)
	}
	results := make([]map[string]any, 0, len(allResults))
	for _, r := range allResults {
		entry := map[string]any{"name": r.GetName(), "applied": r.GetApplied()}
		// The reason a control did not apply is the useful half -- "this camera
		// has no control by that name" is actionable, a bare false is not.
		if d := r.GetDetail(); d != "" {
			entry["detail"] = d
		}
		results = append(results, entry)
	}
	return okResultBounded(map[string]any{"device_id": id, "results": results},
		intParam(req, "max_bytes", 100000)), nil
}

// cameraDeviceID reads the required device_id. Cameras are addressed by the N
// in /dev/videoN, which camera_list reports.
func cameraDeviceID(req mcpgo.CallToolRequest) (uint32, error) {
	n := intParam(req, "device_id", -1)
	if n < 0 {
		return 0, fmt.Errorf("device_id is required (see camera_list)")
	}
	return uint32(n), nil
}

// splitList turns "a,b , c" into [a b c], dropping empties so a trailing comma
// is not a control named "".
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
