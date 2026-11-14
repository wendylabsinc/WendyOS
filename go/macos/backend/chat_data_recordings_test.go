package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type chatRecordingClient struct {
	chatDataClient
	manifests  map[string]data.Manifest
	episodes   []*pb.DataEpisode
	inspectErr error
}

func (c *chatRecordingClient) Episodes(context.Context, *pb.DataEpisodesRequest, ...grpc.CallOption) (*pb.DataEpisodesResponse, error) {
	return &pb.DataEpisodesResponse{Episodes: c.episodes, ReportsInProgress: true}, nil
}
func (c *chatRecordingClient) Inspect(_ context.Context, r *pb.DataInspectRequest, _ ...grpc.CallOption) (*pb.DataInspectResponse, error) {
	if c.inspectErr != nil {
		return nil, c.inspectErr
	}
	raw, _ := json.Marshal(c.manifests[r.Episode])
	return &pb.DataInspectResponse{ManifestJson: raw}, nil
}
func TestChatDataRecordingPagesAndFailures(t *testing.T) {
	ts, _ := time.Parse(time.RFC3339, "2026-11-07T10:00:00Z")
	c := &chatRecordingClient{manifests: map[string]data.Manifest{"a": {ID: "a", StartedUnixNanos: ts.UnixNano(), Trigger: data.EpisodeTrigger{CampaignName: "door"}}}, episodes: []*pb.DataEpisode{{Id: "b", StartedUnixNanos: ts.UnixNano()}, {Id: "a", StartedUnixNanos: ts.UnixNano()}}}
	args := `{"campaign":"door","start":"2026-11-07T00:00:00Z","end":"2026-11-08T00:00:00Z","limit":1}`
	out := runDataTest(t, c, "device_data_recordings", args)
	if out["next_offset"] != float64(1) || len(out["recordings"].([]any)) != 1 {
		t.Fatal(out)
	}
	out = runDataTest(t, c, "device_data_recordings", `{"start":"2026-11-07T00:00:00Z","end":"2026-11-08T00:00:00Z","offset":1}`)
	if len(out["unavailable"].([]any)) != 1 {
		t.Fatal(out)
	}
}
func TestChatDataRecordingCardRequiresVerifiedIdentity(t *testing.T) {
	c := &chatRecordingClient{manifests: map[string]data.Manifest{"a": {ID: "a", Name: "Door"}}}
	tools := chatDataForTest(c)
	tools.turn = &ChatTurn{}
	call := chat.ToolCall{Name: "device_data_recording", Arguments: json.RawMessage(`{"episode":"a","show":true}`)}
	for range 2 {
		if _, err := tools.Execute(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}
	if len(tools.turn.Cards) != 1 || tools.turn.Cards[0].Episode != "a" {
		t.Fatal(tools.turn.Cards)
	}
	c.manifests["a"] = data.Manifest{ID: "other"}
	if _, err := tools.Execute(context.Background(), call); err == nil {
		t.Fatal("accepted wrong manifest")
	}
	c.inspectErr = errors.New("offline")
	if _, err := tools.Execute(context.Background(), call); err == nil {
		t.Fatal("silenced offline")
	}
}

func TestChatDataRecordingOmitsWebhookCredentials(t *testing.T) {
	c := &chatRecordingClient{manifests: map[string]data.Manifest{"a": {ID: "a", Trigger: data.EpisodeTrigger{Notify: &data.CampaignNotify{On: "detection", Webhook: "https://example.invalid/secret"}}}}}
	out := runDataTest(t, c, "device_data_recording", `{"episode":"a"}`)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "webhook_configured") {
		t.Fatal(string(raw))
	}
}
