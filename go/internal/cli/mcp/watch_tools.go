package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Camera watches (design §6.5): a detector runs on the device and this session
// hears when one of the watched classes appears. Nothing is recorded or
// uploaded, and watches end with the session.
func (s *mcpServer) registerWatchTools(srv *server.MCPServer) {
	add := func(name string, handler server.ToolHandlerFunc, opts ...mcpgo.ToolOption) {
		opts = append(opts, localOnly()...)
		srv.AddTool(mcpgo.NewTool(name, opts...), handler)
	}
	add("watch_sources", s.handleWatchSources, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("List what a camera watch can use: healthy cameras on the connected device (id, name), the detector watches run (id, model, class labels), and free watch slots. Call before watch_start."),
	}, readOnly()...)...)
	add("watch_start", s.handleWatchStart, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Watch a camera on the connected device for some classes of object, for example a person at the door. A detector runs on the device and this session is notified when a watched class appears; nothing is recorded or uploaded. At most two watches run per session, and they end when the session ends. Returns when the watch is READY, ERROR or ENDED, or after 30 s while it is still PREPARING: a first watch on a device installs the detector, which takes minutes. ENDED means the device connection changed during the start."),
		mcpgo.WithString("camera", mcpgo.Description("Camera id from watch_sources"), mcpgo.Required()),
		mcpgo.WithArray("classes", mcpgo.Description(`1-10 class labels from the detector's labels in watch_sources, for example ["person"]`), mcpgo.Required(), mcpgo.WithStringItems(), mcpgo.MinItems(1), mcpgo.MaxItems(10)),
		mcpgo.WithNumber("min_confidence", mcpgo.Description("Minimum detection score, 0.3 to 0.95; default 0.5"), mcpgo.Min(0.3), mcpgo.Max(0.95)),
		mcpgo.WithString("label", mcpgo.Description(`Short name shown to the user, for example "front door"; default the camera's name`), mcpgo.MaxLength(64)),
	}, mutating()...)...)
	add("watch_list", s.handleWatchList, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("List this session's camera watches with state, classes and last event time, including recently ended ones and why they ended."),
	}, readOnly()...)...)
	add("watch_stop", s.handleWatchStop, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Stop a camera watch and remove its detector from the device."),
		mcpgo.WithString("watch_id", mcpgo.Description("Watch id from watch_start or watch_list"), mcpgo.Required()),
	}, append(mutating(), idempotent()...)...)...)
	add("watch_events", s.handleWatchEvents, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Read a watch's events after a sequence number from its buffer of the last 100, with its current state. Pass the previous result's next_sequence as after_sequence: otherwise every call returns the same buffered events at once instead of waiting. wait_seconds waits for the next event. For clients that do not show this server's watch notifications."),
		mcpgo.WithString("watch_id", mcpgo.Description("Watch id from watch_start or watch_list"), mcpgo.Required()),
		mcpgo.WithNumber("after_sequence", mcpgo.Description("Return events after this sequence number: the previous result's next_sequence; default 0 (all buffered)"), mcpgo.Min(0)),
		mcpgo.WithNumber("wait_seconds", mcpgo.Description("Wait up to this long for a new event, 0 to 120; default 0"), mcpgo.Min(0), mcpgo.Max(120)),
	}, readOnly()...)...)
}

func (s *mcpServer) watchManager() *watchManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watches
}

func (s *mcpServer) connWithRevision() (*grpcclient.AgentConnection, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connRevision
}

type watchCamera struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func watchCameras(ctx context.Context, conn *grpcclient.AgentConnection) ([]watchCamera, error) {
	if conn.DataService == nil {
		return nil, errWatchAgentTooOld
	}
	response, err := conn.DataService.Sources(ctx, &agentpbv2.DataSourcesRequest{})
	if err != nil {
		return nil, watchDeviceError(err)
	}
	cameras := []watchCamera{}
	for _, source := range response.GetSources() {
		if source.GetKind() == "camera" && source.GetHealthy() {
			cameras = append(cameras, watchCamera{ID: source.GetId(), Name: watchCameraName(source.GetId(), source.GetDetail())})
		}
	}
	return cameras, nil
}

// watchCameraName is the camera's own name. The agent's source detail appends
// the transport ("Brio 101 VIDEO_TRANSPORT_USB"), which is not part of it.
func watchCameraName(id, detail string) string {
	name := strings.TrimSpace(detail)
	if i := strings.LastIndex(name, " VIDEO_TRANSPORT_"); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	if name == "" {
		return id
	}
	return name
}

func watchErrorResult(err error) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, errWatchAgentTooOld):
		return errResult(errCodeUnsupported, errWatchAgentTooOld.Error())
	case errors.Is(err, errWatchNotFound):
		return errResult(errCodeNotFound, err.Error())
	case errors.Is(err, errWatchLimit):
		return errResult(errCodeInvalidArgument, err.Error())
	}
	return errResult(codeFromGRPC(err), grpcErrString(err))
}

func (s *mcpServer) handleWatchSources(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	cameras, err := watchCameras(ctx, conn)
	if err != nil {
		return watchErrorResult(err), nil
	}
	d := defaultWatchDetector
	return okResult(map[string]any{"cameras": cameras, "detector": map[string]any{"id": d.ID, "model": d.Model, "labels": d.Labels}, "free_slots": m.freeSlots()}), nil
}

func (s *mcpServer) handleWatchStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	conn, revision := s.connWithRevision()
	if conn == nil {
		return errNotConnected(), nil
	}
	camera := strings.TrimSpace(req.GetString("camera", ""))
	if camera == "" {
		return errResult(errCodeInvalidArgument, "camera is required; call watch_sources for camera ids"), nil
	}
	classes := req.GetStringSlice("classes", nil)
	if len(classes) < 1 || len(classes) > 10 {
		return errResult(errCodeInvalidArgument, "classes must list 1 to 10 of the detector's labels"), nil
	}
	seen := map[string]bool{}
	for _, class := range classes {
		if !defaultWatchDetector.hasLabel(class) {
			return errResultf(errCodeInvalidArgument, "%q is not one of the detector's labels; call watch_sources for them", class), nil
		}
		if seen[class] {
			return errResultf(errCodeInvalidArgument, "%q is listed more than once", class), nil
		}
		seen[class] = true
	}
	confidence := req.GetFloat("min_confidence", 0.5)
	if math.IsNaN(confidence) || confidence < 0.3 || confidence > 0.95 {
		return errResult(errCodeInvalidArgument, "min_confidence must be from 0.3 to 0.95"), nil
	}
	label := strings.TrimSpace(req.GetString("label", ""))
	if len(label) > 64 {
		return errResult(errCodeInvalidArgument, "label must be at most 64 bytes"), nil
	}
	cameras, err := watchCameras(ctx, conn)
	if err != nil {
		return watchErrorResult(err), nil
	}
	var chosen *watchCamera
	known := make([]string, 0, len(cameras))
	for i := range cameras {
		known = append(known, fmt.Sprintf("%s (%s)", cameras[i].ID, cameras[i].Name))
		if cameras[i].ID == camera {
			chosen = &cameras[i]
		}
	}
	if chosen == nil {
		return errResultf(errCodeInvalidArgument, "no healthy camera %q; healthy cameras: %s", camera, strings.Join(known, ", ")), nil
	}
	if label == "" {
		label = chosen.Name
	}
	view, err := m.start(ctx, conn, revision, watchSpec{CameraID: chosen.ID, CameraName: chosen.Name, Classes: classes, MinConfidence: confidence, Label: label})
	if err != nil {
		return watchErrorResult(err), nil
	}
	return okResult(map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "reason": view.Reason, "next_step": watchNextStep(view.State)}), nil
}

func watchNextStep(state string) string {
	switch watchState(state) {
	case watchReady:
		return "The watch is running. Events arrive as notifications; a client that does not show them can call watch_events with wait_seconds, passing each result's next_sequence back as after_sequence. Stop it with watch_stop when the user no longer needs it."
	case watchError:
		return "Tell the user the reason. The device keeps retrying; stop the watch with watch_stop if the user does not want to wait."
	case watchEnded:
		return "The watch ended; its reason says why."
	}
	return "The device is still preparing the detector; a first watch installs it, which takes minutes. READY arrives as a notification, or check with watch_list or watch_events."
}

func (s *mcpServer) handleWatchList(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	return okResult(map[string]any{"watches": m.list(), "free_slots": m.freeSlots()}), nil
}

func (s *mcpServer) handleWatchStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	id := strings.TrimSpace(req.GetString("watch_id", ""))
	if id == "" {
		return errResult(errCodeInvalidArgument, "watch_id is required"), nil
	}
	view, err := m.stop(ctx, id, watchStoppedReason)
	if errors.Is(err, errWatchNotFound) {
		return watchErrorResult(err), nil
	}
	result := map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "removed": err == nil}
	if err != nil {
		result["message"] = "The watch ended here, but removing its detector from the device failed (" + grpcErrString(err) + "); it stops by itself within a minute."
	}
	return okResult(result), nil
}

func (s *mcpServer) handleWatchEvents(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	id := strings.TrimSpace(req.GetString("watch_id", ""))
	if id == "" {
		return errResult(errCodeInvalidArgument, "watch_id is required"), nil
	}
	after := req.GetFloat("after_sequence", 0)
	wait := req.GetFloat("wait_seconds", 0)
	if math.IsNaN(after) || after < 0 || math.IsNaN(wait) || wait < 0 || wait > 120 {
		return errResult(errCodeInvalidArgument, "after_sequence must be at least 0 and wait_seconds from 0 to 120"), nil
	}
	view, events, gap, err := m.events(ctx, id, uint64(after), time.Duration(wait*float64(time.Second)))
	if err != nil {
		return watchErrorResult(err), nil
	}
	return okResult(map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "reason": view.Reason, "events": events, "gap": gap, "next_sequence": view.LastSequence}), nil
}

// startWatches gives the server its watch manager for the life of Start. The
// returned function removes every active watch when the server exits.
func (s *mcpServer) startWatches(srv *server.MCPServer) func() {
	notify := func(method string, params map[string]any) error {
		return srv.SendNotificationToSpecificClient("stdio", method, params)
	}
	m := newWatchManager(newCampaignWatchBackend(defaultWatchDetector), notify, func() uint64 {
		_, revision := s.connWithRevision()
		return revision
	})
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	return func() {
		m.shutdown()
		s.mu.Lock()
		if s.watches == m {
			s.watches = nil
		}
		s.mu.Unlock()
	}
}
