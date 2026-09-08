package commands

import (
	"bytes"
	"strings"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestParseControlAssignments(t *testing.T) {
	got, err := parseControlAssignments([]string{"auto_exposure=1", "exposure_time_absolute=20"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 ||
		got[0].GetName() != "auto_exposure" || got[0].GetValue() != 1 ||
		got[1].GetName() != "exposure_time_absolute" || got[1].GetValue() != 20 {
		t.Fatalf("parsed wrong: %+v", got)
	}
}

func TestParseControlAssignments_Errors(t *testing.T) {
	for _, arg := range []string{"noequals", "=5", "gain=notanumber", "gain="} {
		if _, err := parseControlAssignments([]string{arg}); err == nil {
			t.Errorf("expected error for %q, got nil", arg)
		}
	}
}

func TestReportControlResults_AllApplied(t *testing.T) {
	var buf bytes.Buffer
	err := reportControlResults(&buf, 0, []*agentpb.CameraControlResult{
		{Name: "auto_exposure", Applied: true},
		{Name: "exposure_time_absolute", Applied: true},
	})
	if err != nil {
		t.Fatalf("want nil error when all applied, got %v", err)
	}
	if !strings.Contains(buf.String(), "auto_exposure applied") {
		t.Fatalf("output missing confirmation: %q", buf.String())
	}
}

func TestReportControlResults_PartialFailureIsAnError(t *testing.T) {
	var buf bytes.Buffer
	err := reportControlResults(&buf, 2, []*agentpb.CameraControlResult{
		{Name: "auto_exposure", Applied: true},
		{Name: "exposure_time_absolute", Applied: false, Detail: "invalid argument"},
	})
	if err == nil {
		t.Fatalf("want an error when a control fails")
	}
	if !strings.Contains(err.Error(), "exposure_time_absolute") {
		t.Fatalf("error should name the failed control: %v", err)
	}
	if !strings.Contains(buf.String(), "NOT applied (invalid argument)") {
		t.Fatalf("output missing failure detail: %q", buf.String())
	}
}
