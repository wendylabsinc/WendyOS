package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEventsWatchFilter(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	for _, n := range []data.CampaignNotification{{Campaign: "people"}, {Watch: "chat-1", Inference: "chat-1", Leased: true}} {
		n.ID, n.Event, n.OccurredAt = uuid.NewString(), "front-door", time.Now().UTC().Format(time.RFC3339Nano)
		if err := manager.RecordNotification(n); err != nil {
			t.Fatal(err)
		}
	}
	read := func(watches ...string) []data.CampaignNotification {
		t.Helper()
		response, err := service.Events(context.Background(), &agentpbv2.DataEventsRequest{NotificationsOnly: true, Replay: true, Watches: watches})
		if err != nil {
			t.Fatal(err)
		}
		var items []data.CampaignNotification
		if err := json.Unmarshal(response.GetEventsJson(), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	if items := read(); len(items) != 1 || items[0].Campaign != "people" {
		t.Fatalf("unfiltered = %+v", items)
	}
	if items := read("chat-1"); len(items) != 1 || items[0].Watch != "chat-1" {
		t.Fatalf("named = %+v", items)
	}
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = "w"
	}
	if _, err := service.Events(context.Background(), &agentpbv2.DataEventsRequest{NotificationsOnly: true, Watches: tooMany}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("17 watches error = %v", err)
	}
	if _, err := service.Events(context.Background(), &agentpbv2.DataEventsRequest{NotificationsOnly: true, Watches: []string{"../x"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad watch name error = %v", err)
	}
}
