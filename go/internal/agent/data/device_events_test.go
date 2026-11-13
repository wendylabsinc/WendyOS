package data

import (
	"fmt"
	"testing"
)

func TestDeviceEventCursorPersistsAndFilters(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, gap, err := m.DeviceEvents("app", "person_detected", "", false)
	if err != nil || gap {
		t.Fatal(err, gap)
	}
	for _, app := range []string{"other", "app"} {
		if err := m.appendDeviceEvent(app, ApplicationRecord{Type: "event", Name: "person_detected", Attributes: map[string]any{"confidence": .94}}, "boot", 10); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	events, next, gap, err := restarted.DeviceEvents("app", "person_detected", cursor, false)
	if err != nil || gap || len(events) != 1 || events[0].AppID != "app" || events[0].ID == "" {
		t.Fatalf("events=%v gap=%v error=%v", events, gap, err)
	}
	again, _, _, err := restarted.DeviceEvents("app", "person_detected", next, false)
	if err != nil || len(again) != 0 {
		t.Fatal("cursor replayed event", again, err)
	}
	if _, _, _, err := restarted.DeviceEvents("app", "", "bad", false); err == nil {
		t.Fatal("accepted invalid cursor")
	}
}
func TestDeviceEventRetentionGap(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.appendDeviceEvent("app", ApplicationRecord{Type: "event", Name: "person"}, "boot", 1); err != nil {
		t.Fatal(err)
	}
	_, cursor, _, _ := m.DeviceEvents("app", "", "", false)
	for i := 0; i < 513; i++ {
		if err := m.appendDeviceEvent("app", ApplicationRecord{Type: "event", Name: fmt.Sprint(i)}, "boot", int64(i+2)); err != nil {
			t.Fatal(err)
		}
	}
	events, _, gap, err := m.DeviceEvents("app", "", cursor, false)
	if err != nil || !gap || len(events) != 512 {
		t.Fatalf("len=%d gap=%v err=%v", len(events), gap, err)
	}
}
