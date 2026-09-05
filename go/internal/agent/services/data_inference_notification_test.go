package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if err := (&CampaignWebhookSender{}).Send(context.Background(), server.URL, notification); err != nil {
		t.Fatal(err)
	}
	if actual := <-received; actual != notification {
		t.Fatalf("notification metadata changed: %+v", actual)
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
