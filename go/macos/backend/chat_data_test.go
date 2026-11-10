package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type chatDataClient struct {
	pb.DataServiceClient
	events  []data.DeviceEvent
	request *pb.DataEventsRequest
	gap     bool
	err     error
}

func (c *chatDataClient) Events(_ context.Context, req *pb.DataEventsRequest, _ ...grpc.CallOption) (*pb.DataEventsResponse, error) {
	c.request = req
	raw, _ := json.Marshal(c.events)
	return &pb.DataEventsResponse{EventsJson: raw, Gap: c.gap, Cursor: "epoch:99"}, c.err
}
func chatDataForTest(c pb.DataServiceClient) *deviceDataTools {
	return &deviceDataTools{Executor: &testDesktopChatTools{}, conn: &grpcclient.AgentConnection{DataService: c}}
}
func runDataTest(t *testing.T, c pb.DataServiceClient, name, args string) map[string]any {
	t.Helper()
	text, err := chatDataForTest(c).Execute(context.Background(), chat.ToolCall{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out["device_scope"] != "original_chat_device" {
		t.Fatal(out)
	}
	return out["evidence"].(map[string]any)
}
func TestChatDataEventsWindowFiltersAndCoverage(t *testing.T) {
	c := &chatDataClient{gap: true, events: []data.DeviceEvent{
		{ID: "end", AppID: data.CampaignAppID("door"), Name: "person", ObservedAt: "2026-11-07T12:00:00Z"},
		{ID: "one", AppID: data.CampaignAppID("door"), Name: "person", ObservedAt: "2026-11-07T10:00:00Z", Attributes: map[string]any{"source_id": "front", "confidence": .9}},
		{ID: "two", AppID: data.CampaignAppID("door"), Name: "person", ObservedAt: "2026-11-07T11:00:00Z", Inputs: []data.SampleRef{{SourceID: "front"}}},
		{ID: "other", AppID: "other", Name: "person", ObservedAt: "2026-11-07T11:00:00Z"},
	}}
	out := runDataTest(t, c, "device_data_events", `{"campaign":"door","source_id":"front","start":"2026-11-07T10:00:00Z","end":"2026-11-07T12:00:00Z","limit":1}`)
	if !c.request.Replay || c.request.AppId != data.CampaignAppID("door") {
		t.Fatal(c.request)
	}
	if out["history_complete"] != false || out["journal_gap"] != true || out["truncated"] != true || out["matching_retained_events"] != float64(2) {
		t.Fatal(out)
	}
	if got := out["events"].([]any)[0].(map[string]any)["id"]; got != "two" {
		t.Fatal(got)
	}
}
func TestChatDataRejectsInvalidAndCancelledBeforeRPC(t *testing.T) {
	for _, args := range []string{`null`, `{} {}`, `{"limit":101}`, `{"start":"yesterday"}`, `{"start":"2026-11-08T00:00:00Z","end":"2026-11-07T00:00:00Z"}`, `{"unknown":true}`, `{"campaign":"a","app_id":"b"}`} {
		c := &chatDataClient{}
		_, err := chatDataForTest(c).Execute(context.Background(), chat.ToolCall{Name: "device_data_events", Arguments: json.RawMessage(args)})
		if err == nil || c.request != nil {
			t.Fatalf("accepted %s: %v", args, err)
		}
	}
	c := &chatDataClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := chatDataForTest(c).Execute(ctx, chat.ToolCall{Name: "device_data_events", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, context.Canceled) || c.request != nil {
		t.Fatal(err)
	}
}
func TestChatDataFailuresAreNotEmptySuccess(t *testing.T) {
	c := &chatDataClient{err: errors.New("device offline")}
	_, err := chatDataForTest(c).Execute(context.Background(), chat.ToolCall{Name: "device_data_events", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatal(err)
	}
	c = &chatDataClient{events: []data.DeviceEvent{{ID: "bad", ObservedAt: "not a timestamp"}}}
	out := runDataTest(t, c, "device_data_events", `{}`)
	if out["invalid_timestamp_count"] != float64(1) || out["history_complete"] != false {
		t.Fatal(out)
	}
}
func TestChatDataToolsAreReadOnly(t *testing.T) {
	tools, err := chatDataForTest(&chatDataClient{}).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "device_data_") && tool.RequiresApproval {
			t.Fatalf("read requires approval: %s", tool.Name)
		}
	}
}

func TestChatDataEventQueryRunsWithoutApprovalInNativeToolChain(t *testing.T) {
	client := &chatDataClient{events: []data.DeviceEvent{}}
	wrapped := &deviceCardTools{Executor: &deviceCampaignTools{Executor: chatDataForTest(client)}, turn: &ChatTurn{}}
	provider := &deviceToolTestProvider{call: chat.ToolCall{Name: "device_data_events", Arguments: json.RawMessage(`{}`)}}
	engine := chat.NewEngine(provider, wrapped, deviceChatPrompt())
	approvals := 0
	err := engine.Turn(context.Background(), "What happened recently?", nil, func(context.Context, chat.ToolCall) (bool, error) { approvals++; return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if approvals != 0 || client.request == nil {
		t.Fatalf("approvals=%d request=%v", approvals, client.request)
	}
	found := false
	for _, message := range engine.Messages() {
		if strings.Contains(message.Content, `"history_complete":false`) {
			found = true
		}
	}
	if !found {
		t.Fatal("coverage evidence did not reach the model")
	}
}
