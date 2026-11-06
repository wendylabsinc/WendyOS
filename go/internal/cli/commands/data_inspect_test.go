package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An extended recording's part says where it sits in the recording and why it
// ended; any other episode prints exactly as before.
func TestPrintInspectionShowsRecordingParts(t *testing.T) {
	inspect := func(manifest string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		if err := printInspection(cmd, []byte(manifest)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	part := inspect(`{"id": "ep-2", "state": "complete", "trigger": {"reason": "event:fire", "campaign_name": "fire-watch"},
		"extension": {"part": 2, "previous_episode": "ep-1", "next_episode": "ep-3", "max_duration_nanos": 60000000000, "end_reason": "max_duration"}}`)
	for _, want := range []string{
		"Recording part: 2 (max_duration 1m0s); ended by max_duration\n",
		"Continues episode: ep-1\n",
		"Continued in episode: ep-3\n",
	} {
		if !strings.Contains(part, want) {
			t.Errorf("inspection lacks %q:\n%s", want, part)
		}
	}
	fixed := inspect(`{"id": "ep-9", "state": "complete", "trigger": {"reason": "manual"}}`)
	if strings.Contains(fixed, "Recording part") || strings.Contains(fixed, "Continue") {
		t.Fatalf("a fixed-window episode prints recording parts:\n%s", fixed)
	}
}
