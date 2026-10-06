package data

import (
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
