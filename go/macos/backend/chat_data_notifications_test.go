package backend

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type chatNotificationClient struct {
	chatHealthClient
	supported           bool
	notifications       []data.CampaignNotification
	notificationRequest *pb.DataEventsRequest
}

func (c *chatNotificationClient) Events(_ context.Context, r *pb.DataEventsRequest, _ ...grpc.CallOption) (*pb.DataEventsResponse, error) {
	c.notificationRequest = r
	raw, _ := json.Marshal(c.notifications)
	return &pb.DataEventsResponse{Notifications: c.supported, EventsJson: raw, Gap: true}, nil
}
func TestChatDataNotificationsNeverClaimReceipt(t *testing.T) {
	c := &chatNotificationClient{supported: true, notifications: []data.CampaignNotification{{ID: "n", Campaign: "door", OccurredAt: "2026-11-07T10:00:00Z", Count: 2, Detections: []data.NotificationDetection{{Label: "person", Score: .95}}}}}
	out := runDataTest(t, c, "device_data_notifications", `{"campaign":"door","start":"2026-11-07T00:00:00Z","end":"2026-11-08T00:00:00Z","check_cloud_permission":true}`)
	if !c.notificationRequest.NotificationsOnly || !c.notificationRequest.Replay {
		t.Fatal(c.notificationRequest)
	}
	if out["recipient_delivery"] != "unknown" || out["provider_acceptance"] != "unknown" || out["history_complete"] != false {
		t.Fatal(out)
	}
	if len(out["notifications"].([]any)) != 1 || out["unavailable"].(map[string]any)["cloud_permission"] == nil {
		t.Fatal(out)
	}
}
func TestChatDataNotificationsRejectLegacyJournal(t *testing.T) {
	c := &chatNotificationClient{}
	_, err := chatDataForTest(c).Execute(context.Background(), chat.ToolCall{Name: "device_data_notifications", Arguments: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("legacy raw events treated as notification intents")
	}
}
