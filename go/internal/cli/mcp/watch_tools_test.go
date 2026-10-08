package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var watchToolNames = []string{"watch_sources", "watch_start", "watch_list", "watch_stop", "watch_events"}

// watchToolServer is a server connected to a fake device, with a fast backend
// and a notification log in place of the stdio client.
func watchToolServer(t *testing.T) (*mcpServer, *watchDataClient, *notificationLog) {
	t.Helper()
	client := newWatchDataClient()
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{DataService: client})
	log := &notificationLog{}
	m := newWatchManager(fastCampaignBackend(), log.notify, func() uint64 { _, r := s.connWithRevision(); return r })
	m.startWait = 2 * time.Second
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	t.Cleanup(m.shutdown)
	return s, client, log
}

func callWatchTool(t *testing.T, s *mcpServer, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	handlers := map[string]server.ToolHandlerFunc{
		"watch_sources": s.handleWatchSources, "watch_start": s.handleWatchStart, "watch_list": s.handleWatchList,
		"watch_stop": s.handleWatchStop, "watch_events": s.handleWatchEvents,
	}
	result, err := handlers[name](context.Background(), callToolReq(name, args))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	return out, result.IsError
}

func TestWatchToolsAreHardwareToolsWithTheRightAnnotations(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range watchToolNames {
		tool := srv.GetTool(name)
		if tool == nil || !slices.Contains(toolGroups["hardware"], name) {
			t.Fatalf("%s is not a registered hardware tool", name)
		}
		readOnly := tool.Tool.Annotations.ReadOnlyHint != nil && *tool.Tool.Annotations.ReadOnlyHint
		if mutating := name == "watch_start" || name == "watch_stop"; readOnly == mutating {
			t.Fatalf("%s read-only = %v", name, readOnly)
		}
	}
}

// A client that only polls must pass next_sequence back as after_sequence, or
// every watch_events call returns the same buffered events without waiting.
// watch_start can return ENDED when the device changes during the start.
func TestWatchToolDocsExplainPollingAndEndedStarts(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	events := srv.GetTool("watch_events").Tool.Description
	if !strings.Contains(events, "next_sequence") || !strings.Contains(events, "after_sequence") {
		t.Fatalf("watch_events description %q", events)
	}
	if start := srv.GetTool("watch_start").Tool.Description; !strings.Contains(start, "READY, ERROR or ENDED") {
		t.Fatalf("watch_start description %q", start)
	}
	_, guide, _ := strings.Cut(guideText, "## Camera watches")
	guide, _, _ = strings.Cut(guide, "\n## ")
	for _, want := range []string{"next_sequence", "after_sequence", "ENDED"} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide's Camera watches section does not mention %s", want)
		}
	}
	doc, err := assets.FS.ReadFile("docs/integrations/mcp.mdx")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, "| `watch_events`") && !strings.Contains(line, "next_sequence") {
			t.Errorf("mcp.mdx watch_events row %q", line)
		}
	}
}

func TestWatchStartValidatesBeforeDeploying(t *testing.T) {
	s, client, _ := watchToolServer(t)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no camera", map[string]any{"classes": []any{"person"}}, "camera"},
		{"unknown camera", map[string]any{"camera": "v4l2:/dev/video9", "classes": []any{"person"}}, "v4l2:/dev/video0 (Brio 101)"},
		{"unhealthy camera", map[string]any{"camera": "v4l2:/dev/video2", "classes": []any{"person"}}, "v4l2:/dev/video0 (Brio 101)"},
		{"no classes", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{}}, "1 to 10"},
		{"eleven classes", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person", "car", "dog", "cat", "bus", "boat", "bird", "cow", "horse", "sheep", "bear"}}, "1 to 10"},
		{"unknown class", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"motorcycle"}}, "watch_sources"},
		{"duplicate class", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person", "person"}}, "more than once"},
		{"confidence too low", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.2}, "0.3"},
		{"confidence too high", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.96}, "0.95"},
		{"long label", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "label": strings.Repeat("x", 65)}, "64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr := callWatchTool(t, s, "watch_start", tc.args)
			if !isErr || out["error_code"] != "INVALID_ARGUMENT" || !strings.Contains(out["message"].(string), tc.want) {
				t.Fatalf("got %v %v, want INVALID_ARGUMENT mentioning %q", isErr, out, tc.want)
			}
		})
	}
	if len(client.callLog()) != 0 {
		t.Fatalf("an invalid request reached the device: %v", client.callLog())
	}
}

func TestWatchToolsValidateIDsAndWaits(t *testing.T) {
	s, _, _ := watchToolServer(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"watch_stop", map[string]any{}},
		{"watch_events", map[string]any{}},
		{"watch_events", map[string]any{"watch_id": "w1", "wait_seconds": 121}},
		{"watch_events", map[string]any{"watch_id": "w1", "after_sequence": -1}},
	} {
		if out, isErr := callWatchTool(t, s, tc.tool, tc.args); !isErr || out["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("%s %v: %v", tc.tool, tc.args, out)
		}
	}
	if out, isErr := callWatchTool(t, s, "watch_events", map[string]any{"watch_id": "w5"}); !isErr || out["error_code"] != "NOT_FOUND" {
		t.Fatalf("unknown watch: %v", out)
	}
	if out, isErr := callWatchTool(t, s, "watch_list", map[string]any{}); isErr || out["free_slots"] != float64(2) {
		t.Fatalf("list: %v", out)
	}
}

func TestWatchToolsNeedAConnection(t *testing.T) {
	s := New(&config.Config{}, nil)
	m := newWatchManager(fastCampaignBackend(), nil, func() uint64 { return 0 })
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	for _, name := range []string{"watch_sources", "watch_start"} {
		if out, isErr := callWatchTool(t, s, name, map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}); !isErr || out["error_code"] != "NOT_CONNECTED" {
			t.Fatalf("%s: %v", name, out)
		}
	}
}

func TestWatchToolsEndToEnd(t *testing.T) {
	s, client, log := watchToolServer(t)
	sources, _ := callWatchTool(t, s, "watch_sources", nil)
	cameras := sources["cameras"].([]any)
	if len(cameras) != 1 || cameras[0].(map[string]any)["name"] != "Brio 101" || sources["free_slots"] != float64(2) {
		t.Fatalf("sources %v", sources)
	}
	if detector := sources["detector"].(map[string]any); detector["id"] != "dfine-nano-coco" || len(detector["labels"].([]any)) != 80 {
		t.Fatalf("detector %v", detector)
	}
	started, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.6})
	if isErr || started["watch_id"] != "w1" || started["label"] != "Brio 101" || started["state"] != "READY" || started["next_step"] == "" {
		t.Fatalf("start %v", started)
	}
	names := client.deployedNames()
	if len(names) != 1 {
		t.Fatalf("deployed %v", names)
	}
	name := names[0]
	client.publish(name, data.NotificationDetection{Label: "person", Score: 0.91})
	events, _ := callWatchTool(t, s, "watch_events", map[string]any{"watch_id": "w1", "wait_seconds": 2})
	list := events["events"].([]any)
	if len(list) != 1 || events["state"] != "READY" || events["next_sequence"] != float64(1) {
		t.Fatalf("events %v", events)
	}
	log.waitFor(t, func(n sentNotification) bool { return n.method == watchEventMethod && n.params["watch_id"] == "w1" })
	listed, _ := callWatchTool(t, s, "watch_list", nil)
	if watches := listed["watches"].([]any); len(watches) != 1 || listed["free_slots"] != float64(1) {
		t.Fatalf("list %v", listed)
	}
	stopped, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w1"})
	if isErr || stopped["state"] != "ENDED" || stopped["removed"] != true {
		t.Fatalf("stop %v", stopped)
	}
	if _, ok := client.campaign(name); ok {
		t.Fatal("watch_stop left the campaign on the device")
	}
	if out, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w7"}); !isErr || out["error_code"] != "NOT_FOUND" {
		t.Fatalf("unknown watch: %v", out)
	}
}

func TestWatchStopReportsADeviceThatDoesNotAnswer(t *testing.T) {
	s, client, log := watchToolServer(t)
	s.watchManager().stopTimeout = 100 * time.Millisecond
	if _, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}); isErr {
		t.Fatal("start failed")
	}
	client.mu.Lock()
	client.removeWait = 5 * time.Second
	client.mu.Unlock()
	began := time.Now()
	stopped, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w1"})
	if elapsed := time.Since(began); isErr || elapsed > time.Second {
		t.Fatalf("watch_stop took %s: %v", elapsed, stopped)
	}
	if message, _ := stopped["message"].(string); stopped["state"] != "ENDED" || stopped["removed"] != false || !strings.Contains(message, "stops by itself") {
		t.Fatalf("stop %v", stopped)
	}
	if !log.ended() {
		t.Fatal("no ENDED status was sent")
	}
}

func TestWatchStartOnAnOldAgent(t *testing.T) {
	s, client, _ := watchToolServer(t)
	client.deployErr = status.Error(codes.InvalidArgument, "parsing campaign YAML: yaml: unmarshal errors:\n  line 3: field lease not found in type data.Campaign")
	out, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}})
	if !isErr || out["error_code"] != "UNSUPPORTED" || !strings.Contains(out["message"].(string), "wendy device update") {
		t.Fatalf("got %v", out)
	}
	if listed, _ := callWatchTool(t, s, "watch_list", nil); len(listed["watches"].([]any)) != 0 || listed["free_slots"] != float64(2) {
		t.Fatalf("a refused watch must leave nothing: %v", listed)
	}
}

// startWatchInBackground calls watch_start while the device is slow to deploy
// and returns once the watch is listed.
func startWatchInBackground(t *testing.T, s *mcpServer) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.handleWatchStart(context.Background(), callToolReq("watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}))
	}()
	waitState(t, s.watchManager(), "w1", watchPreparing)
	return done
}

func TestWatchStopDuringADeployRemovesTheWatch(t *testing.T) {
	s, client, _ := watchToolServer(t)
	client.deployWait = 200 * time.Millisecond
	started := startWatchInBackground(t, s)
	stopped, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w1"})
	if isErr || stopped["state"] != "ENDED" || stopped["removed"] != true {
		t.Fatalf("stop %v", stopped)
	}
	if removed := client.callLog(); !slices.ContainsFunc(removed, func(call string) bool { return strings.HasPrefix(call, "remove:") }) {
		t.Fatalf("watch_stop reported removed before any removal: %v", removed)
	}
	if names := client.deployedNames(); len(names) != 0 {
		t.Fatalf("watch_stop left %v on the device", names)
	}
	<-started
}

func TestWatchStopDuringALongDeployReportsThePendingRemoval(t *testing.T) {
	s, client, _ := watchToolServer(t)
	s.watchManager().stopTimeout = 50 * time.Millisecond
	client.deployWait = 300 * time.Millisecond
	started := startWatchInBackground(t, s)
	stopped, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w1"})
	message, _ := stopped["message"].(string)
	if isErr || stopped["state"] != "ENDED" || stopped["removed"] != false || !strings.Contains(message, "still setting it up") {
		t.Fatalf("stop %v", stopped)
	}
	<-started
	if names := client.deployedNames(); len(names) != 0 {
		t.Fatalf("the late deploy was left on the device: %v", names)
	}
}

func TestWatchStartOnABusyDevice(t *testing.T) {
	s, client, _ := watchToolServer(t)
	s.watchManager().backend.(*campaignWatchBackend).deployTimeout = 50 * time.Millisecond
	client.deployWait = 5 * time.Second
	out, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}})
	message, _ := out["message"].(string)
	if !isErr || out["error_code"] != "TIMEOUT" || !strings.Contains(message, "busy") || !strings.Contains(message, "removes itself") {
		t.Fatalf("got %v", out)
	}
	if listed, _ := callWatchTool(t, s, "watch_list", nil); len(listed["watches"].([]any)) != 0 || listed["free_slots"] != float64(2) {
		t.Fatalf("a refused watch must leave nothing: %v", listed)
	}
}

func TestWatchesEndWhenTheDeviceChanges(t *testing.T) {
	s, _, _ := watchToolServer(t)
	if _, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}); isErr {
		t.Fatal("start failed")
	}
	s.SetConn(&grpcclient.AgentConnection{DataService: newWatchDataClient()})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		listed, _ := callWatchTool(t, s, "watch_list", nil)
		if w := listed["watches"].([]any)[0].(map[string]any); w["state"] == "ENDED" && w["reason"] == "device changed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watch did not end after the connection changed")
}

func TestWatchCameraNameDropsTheTransport(t *testing.T) {
	for detail, want := range map[string]string{"Brio 101 VIDEO_TRANSPORT_USB": "Brio 101", "Front door": "Front door", "": "v4l2:/dev/video0"} {
		if got := watchCameraName("v4l2:/dev/video0", detail); got != want {
			t.Fatalf("%q -> %q, want %q", detail, got, want)
		}
	}
}

func TestWatchToolsReportAnAgentWithoutTheDataService(t *testing.T) {
	s, client, _ := watchToolServer(t)
	client.mu.Lock()
	client.sourcesErr = status.Error(codes.Unimplemented, "unknown service wendy.agent.services.v2.DataService")
	client.mu.Unlock()
	for name, args := range map[string]map[string]any{
		"watch_sources": nil,
		"watch_start":   {"camera": "v4l2:/dev/video0", "classes": []any{"person"}},
	} {
		out, isErr := callWatchTool(t, s, name, args)
		if !isErr || out["error_code"] != "UNSUPPORTED" || !strings.Contains(out["message"].(string), "wendy device update") {
			t.Fatalf("%s: %v", name, out)
		}
	}
	if names := client.deployedNames(); len(names) != 0 {
		t.Fatalf("deployed %v", names)
	}
}
