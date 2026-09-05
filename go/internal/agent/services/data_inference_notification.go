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

// CampaignWebhookSender delivers directly from the agent. It deliberately does
// not impersonate an app through CreateNotificationV2: that Cloud API requires
// an app registered to the device. Cloud-only campaigns retain the existing
// notify.on: episode_committed ingestion path.
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
