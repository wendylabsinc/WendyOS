package data

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNotificationsAreDurableFilteredAndSeparateFromRawEvents(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, _, err := manager.Notifications("", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	notification := CampaignNotification{ID: uuid.NewString(), Event: "person_detected", Campaign: "people", SourceID: "camera", Count: 1, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = manager.RecordNotification(notification); err != nil {
		t.Fatal(err)
	}
	if err = manager.RecordNotification(notification); err != nil {
		t.Fatal(err)
	}
	other := notification
	other.ID = uuid.NewString()
	other.Campaign = "other"
	if err = manager.RecordNotification(other); err != nil {
		t.Fatal(err)
	}
	if err = manager.appendDeviceEvent("sh.wendy.campaign.people", ApplicationRecord{Type: "event", Name: "person_detected"}, "boot", 1); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	items, next, gap, err := restarted.Notifications("sh.wendy.campaign.people", "person_detected", cursor, false)
	if err != nil || gap || len(items) != 1 || items[0].ID != notification.ID || items[0].OccurredAt != notification.OccurredAt {
		t.Fatalf("unexpected notification replay: %v gap=%v err=%v", items, gap, err)
	}
	if again, _, _, err := restarted.Notifications("", "", next, false); err != nil || len(again) != 0 {
		t.Fatal("acknowledged cursor replayed", again, err)
	}
	raw, _, _, err := restarted.DeviceEvents("", "", "", true)
	if err != nil || len(raw) != 1 {
		t.Fatal("notification duplicated in raw event stream", raw, err)
	}
	if _, _, _, err := restarted.Notifications("", "", "invalid", false); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestNotificationsReportRetentionGap(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, _, _ := manager.Notifications("", "", "", false)
	for i := 0; i < 513; i++ {
		if err = manager.RecordNotification(CampaignNotification{ID: uuid.NewString(), Event: "event", Campaign: "people", OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	items, _, gap, err := manager.Notifications("", "", cursor, false)
	if err != nil || !gap || len(items) != 512 {
		t.Fatalf("retention len=%d gap=%v err=%v", len(items), gap, err)
	}
}

func TestNotificationDetectionsRoundTrip(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notification := CampaignNotification{ID: uuid.NewString(), Event: "chat-1.detected", Campaign: "chat-1", SourceID: "camera", Count: 1,
		Detections: []NotificationDetection{{Label: "person", Score: 0.91}}, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := manager.RecordNotification(notification); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := manager.Notifications("", "", "", true)
	if err != nil || len(items) != 1 {
		t.Fatalf("notification not retained: %v %v", items, err)
	}
	if len(items[0].Detections) != 1 || items[0].Detections[0] != (NotificationDetection{Label: "person", Score: 0.91}) {
		t.Fatalf("detections did not round-trip: %+v", items[0].Detections)
	}
}

// Labels may be up to 128 bytes and json escapes '<' as six bytes, so five
// detections can exceed the entry limit. The entry must survive with fewer.
func TestNotificationDropsDetectionsToFitLimit(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	label := strings.Repeat("<", 128)
	notification := CampaignNotification{ID: uuid.NewString(), Event: "e", Campaign: "c", SourceID: "camera", Count: 5, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for i := 0; i < 5; i++ {
		notification.Detections = append(notification.Detections, NotificationDetection{Label: label, Score: 0.9 - float64(i)/10})
	}
	if err := manager.RecordNotification(notification); err != nil {
		t.Fatalf("an oversized detection list cost the whole notification: %v", err)
	}
	items, _, _, err := manager.Notifications("", "", "", true)
	if err != nil || len(items) != 1 {
		t.Fatalf("notification not retained: %v %v", items, err)
	}
	got := items[0].Detections
	if len(got) == 0 || len(got) >= 5 {
		t.Fatalf("detections were not trimmed to fit: kept %d", len(got))
	}
	if got[0].Score != 0.9 {
		t.Fatal("trimming dropped the highest-scored detection")
	}
	if raw, _ := json.Marshal(items[0]); len(raw) > 4096 {
		t.Fatalf("stored entry is %d bytes", len(raw))
	}
}
