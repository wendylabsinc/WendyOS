package backend

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type chatHealthClient struct {
	chatRecordingClient
	sourceErr error
}

func (c *chatHealthClient) Sources(context.Context, *pb.DataSourcesRequest, ...grpc.CallOption) (*pb.DataSourcesResponse, error) {
	return &pb.DataSourcesResponse{Sources: []*pb.DataSource{{Id: "camera:front", Healthy: false, UnusableReason: "offline"}}}, c.sourceErr
}
func (c *chatHealthClient) Status(context.Context, *pb.DataStatusRequest, ...grpc.CallOption) (*pb.DataStatusResponse, error) {
	return &pb.DataStatusResponse{ReportsInProgress: true}, nil
}
func (c *chatHealthClient) CampaignInspect(_ context.Context, r *pb.DataCampaignInspectRequest, _ ...grpc.CallOption) (*pb.DataCampaign, error) {
	return &pb.DataCampaign{Name: r.Name, State: "armed", PlanJson: []byte(`{"notify":{"webhook":"https://example.invalid/secret"},"retention":{"local_quota":"1GB"}}`), Warnings: []string{"quota advisory"}}, nil
}
func TestChatDataHealthKeepsUnknownLossesAndPartialFailures(t *testing.T) {
	c := &chatHealthClient{chatRecordingClient: chatRecordingClient{manifests: map[string]data.Manifest{"e": {ID: "e", State: "interrupted", Sources: []data.SourceStats{{Source: data.Source{ID: "front"}, Drops: nil, DropAccounting: "unknown"}}, Upload: data.WorkflowState{State: "failed", LastError: "offline"}}}}, sourceErr: errors.New("source service unavailable")}
	out := runDataTest(t, c, "device_data_health", `{"episode":"e","campaign":"door"}`)
	if out["unavailable"].(map[string]any)["current_sources"] == nil {
		t.Fatal(out)
	}
	episode := out["episode"].(map[string]any)
	source := episode["sources"].([]any)[0].(map[string]any)
	if _, ok := source["drops"]; ok {
		t.Fatal("invented drop count", source)
	}
	if episode["upload"].(map[string]any)["state"] != "failed" || episode["verification_requested"] != false {
		t.Fatal(episode)
	}
	raw, _ := json.Marshal(out["campaign"])
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if fields["notifications_configured"] != true || fields["notify"] != nil {
		t.Fatal(fields)
	}
}
