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
	notification := CampaignNotification{ID: uuid.NewString(), Event: "person_detected", Campaign: "people", SourceID: "camera", Count: 1,
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

// Labels may be up to 128 bytes and encoding/json escapes '<' as six bytes, so
// five detections can exceed the entry limit. The entry must survive with fewer.
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
	if len(got) != 4 {
		t.Fatalf("expected 4 detections kept, got %d", len(got))
	}
	if got[0].Score != 0.9 {
		t.Fatal("trimming dropped the highest-scored detection")
	}
	if raw, _ := json.Marshal(items[0]); len(raw) > 4096 {
		t.Fatalf("stored entry is %d bytes", len(raw))
	}
}

func recordTestNotification(t *testing.T, m *Manager, n CampaignNotification) {
	t.Helper()
	n.ID = uuid.NewString()
	n.Event = "front-door"
	n.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := m.RecordNotification(n); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationsHideLeasedWatchesFromOtherReaders(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recordTestNotification(t, m, CampaignNotification{Campaign: "people"})
	recordTestNotification(t, m, CampaignNotification{Watch: "porch", Inference: "front-door"})
	recordTestNotification(t, m, CampaignNotification{Watch: "chat-1", Inference: "chat-1", Leased: true})
	recordTestNotification(t, m, CampaignNotification{Watch: "chat-2", Inference: "chat-2", Leased: true})

	names := func(items []CampaignNotification) string {
		var out []string
		for _, item := range items {
			out = append(out, item.Campaign+item.Watch)
		}
		return strings.Join(out, ",")
	}
	all, _, _, err := m.Notifications("", "", "", true)
	if err != nil || names(all) != "people,porch" {
		t.Fatalf("unfiltered read = %s, %v", names(all), err)
	}
	mine, _, _, err := m.NotificationsFor("", "", "", true, []string{"chat-1"})
	if err != nil || names(mine) != "chat-1" {
		t.Fatalf("read naming chat-1 = %s, %v", names(mine), err)
	}
	both, _, _, err := m.NotificationsFor("", "", "", true, []string{"porch", "chat-2"})
	if err != nil || names(both) != "porch,chat-2" {
		t.Fatalf("read naming porch and chat-2 = %s, %v", names(both), err)
	}
	byApp, _, _, err := m.Notifications("sh.wendy.watch.porch", "", "", true)
	if err != nil || names(byApp) != "porch" {
		t.Fatalf("read by watch app id = %s, %v", names(byApp), err)
	}
}

func TestRecordNotificationNeedsACampaignOrAWatch(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = m.RecordNotification(CampaignNotification{ID: uuid.NewString(), Event: "e", OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err == nil || !strings.Contains(err.Error(), "invalid notification identity") {
		t.Fatalf("anonymous notification error = %v", err)
	}
	raw, _ := json.Marshal(CampaignNotification{Watch: "porch"})
	if strings.Contains(string(raw), `"campaign"`) {
		t.Fatalf("a watch entry carries an empty campaign: %s", raw)
	}
}
