//go:build integration

package main

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectTaskBindsNamedSubmissionAndOriginal(t *testing.T) {
	trigger := tasking.TriggerRef{SHA256: "exact-original"}
	destination := &missioncontrol.InspectionDestination{Version: 1, Mode: "planar_nearby", FloorY: .24, MaxOffsetM: 2, PlanarConfigSHA256: "exact-map"}
	h := Handoff{ProposalID: "selected", MissionInput: Input{TriggerJSON: raw(trigger), TriggerSHA256: trigger.SHA256}, NavigationTarget: [3]float64{1, .24, 2}, Destination: destination}
	wanted := tasking.Record{Spec: tasking.Spec{ID: "wanted", Proposal: &missioncontrol.ProposalRef{ID: "selected"}, Trigger: &trigger, Target: h.NavigationTarget, InspectionDestination: destination}, Status: mission.Status{Phase: mission.Succeeded}, Result: &tasking.Inspection{TaskID: "wanted"}}
	other := wanted
	other.Spec.ID = "unrelated"
	got, err := selectTask([]tasking.Record{other, wanted}, h, "wanted")
	if err != nil || got.Spec.ID != "wanted" {
		t.Fatal("unrelated success selected", got, err)
	}
	for _, mode := range []string{"absent", "proposal", "trigger", "target", "running"} {
		t.Run(mode, func(t *testing.T) {
			r := wanted
			switch mode {
			case "absent":
				r.Spec.ID = "other"
			case "proposal":
				r.Spec.Proposal = &missioncontrol.ProposalRef{ID: "other"}
			case "trigger":
				r.Spec.Trigger = &tasking.TriggerRef{SHA256: "other"}
			case "target":
				r.Spec.Target[0] = 3
			case "running":
				r.Status.Phase = mission.Running
			}
			if _, err := selectTask([]tasking.Record{r}, h, "wanted"); err == nil {
				t.Fatal("unbound completion accepted")
			}
		})
	}
}

func TestRecordedNativeUIResultMatchesSelectedTask(t *testing.T) {
	directory := os.Getenv("LOCAL_INSPECTION_RECORDED_RUN")
	if directory == "" {
		t.Skip("optional retained local native UI walkthrough")
	}
	var h Handoff
	var record tasking.Record
	var receipt struct {
		TaskID   string `json:"task_id"`
		Original string `json:"selected_original"`
	}
	for name, value := range map[string]any{"commissioned/handoff.json": &h, "result.json": &record, "native-ui/result.json": &receipt} {
		b, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, value); err != nil {
			t.Fatal(err)
		}
	}
	if receipt.Original != h.OriginalEnvelopeSHA256 {
		t.Fatal("UI selected another original")
	}
	got, err := selectTask([]tasking.Record{record}, h, receipt.TaskID)
	if err != nil || got.Result.TaskID != receipt.TaskID {
		t.Fatal("saved result does not belong to UI task", err)
	}
}
