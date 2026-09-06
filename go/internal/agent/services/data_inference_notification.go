package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// DetectionNotification contains metadata only, never camera images or secrets.
type DetectionNotification struct {
	ID       string `json:"id"`
	Event    string `json:"event"`
	Campaign string `json:"campaign"`
	SourceID string `json:"source_id"`
	Model    string `json:"model"`
	Revision string `json:"model_revision"`
	Count    int    `json:"count"`
}

type CampaignNotificationSender interface {
	Send(context.Context, string, DetectionNotification) error
}

// CampaignCloudSender uses the campaign name as its Cloud app identity. Cloud
// must authorize that identity for this device before accepting a notification.
// An explicit webhook continues to select direct HTTP delivery.
type CampaignCloudSender struct {
	Cloud NotificationSender
}

func (s *CampaignCloudSender) Send(ctx context.Context, endpoint string, notification DetectionNotification) error {
	if endpoint != "" {
		return (&CampaignWebhookSender{}).Send(ctx, endpoint, notification)
	}
	if s.Cloud == nil {
		return status.Error(codes.Unavailable, "Cloud campaign notification delivery is unavailable")
	}
	metadata, err := structpb.NewStruct(map[string]any{
		"event":          notification.Event,
		"campaign":       notification.Campaign,
		"source_id":      notification.SourceID,
		"model":          notification.Model,
		"model_revision": notification.Revision,
		"count":          notification.Count,
	})
	if err != nil {
		return err
	}
	appID := "campaign:" + notification.Campaign
	response, err := s.Cloud.CreateNotificationV2(ctx, &cloudpb.CreateNotificationV2Request{
		AppId: &appID,
		Audience: &cloudpb.NotificationAudience{Roles: []cloudpb.OrganizationRole{
			cloudpb.OrganizationRole_ORGANIZATION_ROLE_OWNER,
			cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN,
		}},
		Title:          "Campaign event",
		Body:           fmt.Sprintf("Campaign %s emitted %s.", notification.Campaign, notification.Event),
		Severity:       cloudpb.NotificationSeverity_NOTIFICATION_SEVERITY_INFO,
		DeepLink:       "wendy://live",
		NotificationId: notification.ID,
		Metadata:       metadata,
	})
	if err != nil {
		return err
	}
	if response.GetNotificationId() != notification.ID {
		return status.Error(codes.DataLoss, "Cloud returned a mismatched campaign notification_id")
	}
	return nil
}

// CampaignWebhookSender delivers directly from the agent to an explicit URL.
type CampaignWebhookSender struct{}

func (*CampaignWebhookSender) Send(ctx context.Context, endpoint string, notification DetectionNotification) error {
	payload, err := json.Marshal(notification)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid notification webhook URL")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", notification.ID)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		// A webhook's path/query may contain a token. Do not include its URL in
		// logs or the campaign's inspectable notification error.
		var requestError *url.Error
		if errors.As(err, &requestError) {
			return fmt.Errorf("notification webhook: %w", requestError.Err)
		}
		return errors.New("notification webhook request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notification webhook returned HTTP %d", response.StatusCode)
	}
	return nil
}
