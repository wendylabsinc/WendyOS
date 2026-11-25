package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"testing"
)

func TestScheduleInputAndSignedPolicy(t *testing.T) {
	rooms := []patrol.Room{{ID: "workshop"}, {ID: "office"}}
	path := filepath.Join(t.TempDir(), "schedule.json")
	if err := os.WriteFile(path, []byte(`{"revisit_seconds":{"workshop":30,"office":300}}`), 0600); err != nil {
		t.Fatal(err)
	}
	schedule, err := readSchedule(path, rooms)
	if err != nil {
		t.Fatal(err)
	}
	// Use the same verified fixture path as the real executor, not a replacement signer.
	store, _, _, spec, err := fixture(t.TempDir(), "schedule-test", world.FrameRef{ID: "studio", Revision: "v1"}, rooms, 3600, scheduleDocument{RevisitSeconds: schedule})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var signed authority.SignedInstruction
	if err = json.Unmarshal(record.Proof, &signed); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Patrol struct {
			RevisitSeconds map[string]int `json:"revisit_seconds"`
		} `json:"patrol"`
	}
	if err = json.Unmarshal(signed.Payload, &payload); err != nil || payload.Patrol.RevisitSeconds["workshop"] != 30 || payload.Patrol.RevisitSeconds["office"] != 300 {
		t.Fatal("schedule missing from immutable signed bytes", err)
	}
	for _, raw := range []string{`{"revisit_seconds":{"workshop":30}}`, `{"revisit_seconds":{"workshop":30,"other":300}}`, `{"revisit_seconds":{"workshop":0,"office":300}}`, `{"revisit_seconds":{"workshop":30,"office":300},"speed":9}`, `{"revisit_seconds":{"workshop":30,"office":300}} {}`} {
		_ = os.WriteFile(path, []byte(raw), 0600)
		if _, err = readSchedule(path, rooms); err == nil {
			t.Fatal("invalid schedule accepted", raw)
		}
	}
}
