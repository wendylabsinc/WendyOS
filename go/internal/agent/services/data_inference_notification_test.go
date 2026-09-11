package services

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCampaignWebhookSendsDetectionMetadata(t *testing.T) {
	notification := DetectionNotification{ID: "event-id", Event: "person_detected", Campaign: "people", SourceID: "v4l2:/dev/video0", Model: "facebook/detr-resnet-50", Revision: "revision", Count: 2}
	received := make(chan DetectionNotification, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != notification.ID {
			t.Error("incorrect webhook protocol")
		}
		var payload DetectionNotification
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := (&CampaignCloudSender{}).Send(context.Background(), server.URL, notification); err != nil {
		t.Fatal(err)
	}
	if actual := <-received; actual != notification {
		t.Fatalf("notification metadata changed: %+v", actual)
	}
}

func TestCampaignCloudNotificationUsesCampaignIdentity(t *testing.T) {
	cloud := &recordingNotificationSender{}
	notification := DetectionNotification{ID: uuid.NewString(), Event: "person_detected", Campaign: "people-all-cameras", SourceID: "v4l2:/dev/video0", Model: "facebook/detr-resnet-50", Revision: "pinned-revision", Count: 2}
	if err := (&CampaignCloudSender{Cloud: cloud}).Send(context.Background(), "", notification); err != nil {
		t.Fatal(err)
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("sent %d Cloud requests", len(cloud.requests))
	}
	request := cloud.requests[0]
	if request.GetAppId() != "campaign:"+notification.Campaign || request.GetNotificationId() != notification.ID || request.OrganizationId != nil {
		t.Fatalf("incorrect source attribution: %v", request)
	}
	if request.GetDeepLink() != "wendy://live" || request.GetSeverity() != cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_INFO {
		t.Fatalf("incorrect alert presentation: %v", request)
	}
	roles := request.GetAudience().GetRoles()
	if len(roles) != 2 || roles[0] != cloudpb.OrganizationRole_ORGANIZATION_ROLE_OWNER || roles[1] != cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN {
		t.Fatalf("unexpected campaign audience: %v", roles)
	}
	metadata := request.GetMetadata().AsMap()
	if metadata["campaign"] != notification.Campaign || metadata["event"] != notification.Event || metadata["source_id"] != notification.SourceID || metadata["model"] != notification.Model || metadata["model_revision"] != notification.Revision || metadata["count"] != float64(notification.Count) {
		t.Fatalf("lost detection metadata: %v", metadata)
	}
}

func TestCampaignCloudNotificationUnavailableAndMismatchedID(t *testing.T) {
	request := DetectionNotification{ID: uuid.NewString(), Campaign: "people", Event: "person_detected"}
	if err := (&CampaignCloudSender{}).Send(context.Background(), "", request); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing Cloud sender: %v", err)
	}
	if err := (&CampaignCloudSender{Cloud: mismatchedNotificationSender{}}).Send(context.Background(), "", request); status.Code(err) != codes.DataLoss {
		t.Fatalf("mismatched Cloud acknowledgement: %v", err)
	}
}

func TestCampaignWebhookRefusesRedirectsAndHidesEndpointSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			t.Error("webhook followed redirect")
		}
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	sender := &CampaignWebhookSender{}
	err := sender.Send(context.Background(), server.URL+"/secret-token", DetectionNotification{ID: "id"})
	if err == nil || !strings.Contains(err.Error(), "307") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("bad redirect error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = sender.Send(ctx, server.URL+"/secret-token", DetectionNotification{ID: "id"})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("request error exposed webhook URL: %v", err)
	}
}

// deniedCampaignCloud records attempts while retaining the real campaign router.
type deniedCampaignCloud struct{ calls atomic.Int32 }

func (s *deniedCampaignCloud) CreateNotificationV2(context.Context, *cloudpb.CreateNotificationV2Request) (*cloudpb.CreateNotificationV2Response, error) {
	s.calls.Add(1)
	return nil, status.Error(codes.PermissionDenied, "campaign grant is disabled")
}

func TestCampaignCloudAuthorizationFailureIsTerminal(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := data.ParseCampaign(inferenceTestYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	campaign.Notify.Webhook = ""
	cloud := &deniedCampaignCloud{}
	job := &campaignInferenceJob{owner: &campaignInferenceManager{service: NewDataService(manager), sender: &CampaignCloudSender{Cloud: cloud}}, campaign: campaign}
	queue := make(chan DetectionNotification, 1)
	queue <- detectionNotification(campaign, "camera", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); job.notifications(ctx, queue) }()
	deadline := time.Now().Add(time.Second)
	var reported string
	for time.Now().Before(deadline) {
		job.mu.Lock()
		reported = job.status.NotificationError
		job.mu.Unlock()
		if reported != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if reported == "" || !strings.Contains(reported, "campaign grant is disabled") {
		t.Fatalf("missing immediate authorization error: %q", reported)
	}
	if cloud.calls.Load() != 1 {
		t.Fatalf("authorization failure retried %d times", cloud.calls.Load())
	}
}
