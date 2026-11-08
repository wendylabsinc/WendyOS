package data

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func episodeEvents(t *testing.T, m *Manager, id string) string {
	t.Helper()
	dir, err := m.episodeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRecordApplicationForReachesOnlyItsCollections(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"clips", "other"} {
		if _, err := m.Start(StartOptions{Sources: []string{"applications"}, Trigger: EpisodeTrigger{Reason: "event:x", CampaignName: key}}); err != nil {
			t.Fatal(err)
		}
	}
	record := ApplicationRecord{Version: 1, Type: "prediction", Model: "m", Attributes: map[string]any{"marker": "for-clips"}}
	if _, err := m.RecordApplicationFor("sh.wendy.inference.front-door", record, map[string]bool{"clips": true}, false, false); err != nil {
		t.Fatal(err)
	}
	clips, err := m.Stop("clips")
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Stop("other")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(episodeEvents(t, m, clips.ID), "for-clips") {
		t.Fatal("the named collection's episode lacks the record")
	}
	if strings.Contains(episodeEvents(t, m, other.ID), "for-clips") {
		t.Fatal("another collection's episode received the record")
	}
}

func TestRecordApplicationForPreRollReachesOnlyItsCollections(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := ApplicationRecord{Version: 1, Type: "event", Name: "front-door", Attributes: map[string]any{"marker": "pre-roll"}}
	if _, err := m.RecordApplicationFor("sh.wendy.inference.front-door", record, map[string]bool{"clips": true}, false, false); err != nil {
		t.Fatal(err)
	}
	opts := func(key string) StartOptions {
		return StartOptions{Sources: []string{"applications"}, PreRollDuration: 10 * time.Second, Trigger: EpisodeTrigger{Reason: "event:front-door", CampaignName: key}}
	}
	clips := recordEpisode(t, m, opts("clips"))
	other := recordEpisode(t, m, opts("other"))
	if !strings.Contains(episodeEvents(t, m, clips.ID), "pre-roll") {
		t.Fatal("the named collection's later episode lacks the pre-roll record")
	}
	if strings.Contains(episodeEvents(t, m, other.ID), "pre-roll") {
		t.Fatal("another collection's later episode took the pre-roll record")
	}
}

func TestRecordApplicationForWithoutAudienceStoresNoPrediction(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	m.SetApplicationObserver(func(_ string, record ApplicationRecord) { observed = append(observed, record.Type+":"+record.Name) })
	prediction := ApplicationRecord{Version: 1, Type: "prediction", Model: "m", Attributes: map[string]any{"marker": "nobody"}}
	if _, err := m.RecordApplicationFor("sh.wendy.inference.chat-1", prediction, nil, true, false); err != nil {
		t.Fatal(err)
	}
	event := ApplicationRecord{Version: 1, Type: "event", Name: "chat-1", Attributes: map[string]any{"marker": "nobody"}}
	if _, err := m.RecordApplicationFor("sh.wendy.inference.chat-1", event, map[string]bool{}, true, false); err != nil {
		t.Fatal(err)
	}
	later := recordEpisode(t, m, StartOptions{Sources: []string{"applications"}, PreRollDuration: 10 * time.Second, Trigger: EpisodeTrigger{Reason: "event:x", CampaignName: "later"}})
	if strings.Contains(episodeEvents(t, m, later.ID), "nobody") {
		t.Fatal("a record with no audience reached a later episode")
	}
	if strings.Join(observed, ",") != "event:chat-1" {
		t.Fatalf("observer saw %v; want only the event", observed)
	}
	events, _, _, err := m.DeviceEvents("", "chat-1", "", true)
	if err != nil || len(events) != 1 {
		t.Fatalf("device events = %v, %v; the event must still be journaled", events, err)
	}
}

func TestPrivateRecordApplicationForKeepsItsEventOffTheJournal(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	m.SetApplicationObserver(func(_ string, record ApplicationRecord) { observed = append(observed, record.Type+":"+record.Name) })
	event := ApplicationRecord{Version: 1, Type: "event", Name: "chat-1"}
	if _, err := m.RecordApplicationFor("sh.wendy.inference.chat-1", event, nil, true, true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(observed, ",") != "event:chat-1" {
		t.Fatalf("observer saw %v; a private event must still reach triggers", observed)
	}
	events, _, _, err := m.DeviceEvents("", "", "", true)
	if err != nil || len(events) != 0 {
		t.Fatalf("device events = %v, %v; a private event must not be journaled", events, err)
	}
}
