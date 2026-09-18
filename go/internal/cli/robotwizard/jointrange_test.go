package robotwizard

import (
	"strings"
	"testing"
	"time"
)

func TestParseJointRangeParams(t *testing.T) {
	tests := []struct {
		name      string
		in        map[string]string
		wantFloor float64
		wantPer   map[string]float64
		errSubstr string
	}{
		{
			name:      "defaults",
			in:        nil,
			wantFloor: 0,
		},
		{
			name:      "a floor and a per-joint override",
			in:        map[string]string{"min_travel": "1000", "min_travel_per_joint": "gripper=300, wrist_roll=500"},
			wantFloor: 1000,
			wantPer:   map[string]float64{"gripper": 300, "wrist_roll": 500},
		},
		{
			// The boundary between parameterising a method and describing one.
			name:      "a key the method does not define",
			in:        map[string]string{"then_open_the_gripper": "true"},
			errSubstr: "unknown parameter",
		},
		{
			name:      "a floor that is not a number",
			in:        map[string]string{"min_travel": "quite far"},
			errSubstr: "min_travel",
		},
		{
			name:      "a malformed per-joint override",
			in:        map[string]string{"min_travel_per_joint": "gripper"},
			errSubstr: "joint=value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJointRangeParams(tt.in)
			if tt.errSubstr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %q, got none", tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.MinTravel != tt.wantFloor {
				t.Errorf("min travel = %v, want %v", got.MinTravel, tt.wantFloor)
			}
			for joint, want := range tt.wantPer {
				if got.MinTravelPerJoint[joint] != want {
					t.Errorf("floor for %q = %v, want %v", joint, got.MinTravelPerJoint[joint], want)
				}
			}
			if got.MinSamples < 2 {
				t.Errorf("min samples = %d; two readings can span anything and have measured nothing", got.MinSamples)
			}
			if got.SampleInterval <= 0 {
				t.Errorf("sample interval = %v, want a positive default", got.SampleInterval)
			}
		})
	}
}

func TestSampleWindowTracksExtremesAndDirection(t *testing.T) {
	w := NewSampleWindow()
	for _, v := range []float64{0, 1, 5, 3, -2, 4} {
		w.Add(JointReading{
			At:        time.Unix(0, 0),
			Positions: map[string]float64{"a": v, "b": 0},
			Order:     []string{"a", "b"},
		})
	}
	span, ok := w.Span("a")
	if !ok || span.Min != -2 || span.Max != 5 {
		t.Fatalf("span = %+v, want -2..5", span)
	}
	if got := w.Travel("a"); got != 7 {
		t.Fatalf("travel = %v, want 7", got)
	}
	if got := w.DirectionSign("a"); got != 1 {
		t.Fatalf("direction = %d, want +1: the value ended above where it started", got)
	}
	if got := w.DirectionSign("b"); got != 0 {
		t.Fatalf("direction = %d, want 0 for a joint that never moved", got)
	}
	if moved, travel := w.MostMoved(); moved != "a" || travel != 7 {
		t.Fatalf("most moved = %q (%v), want a (7)", moved, travel)
	}
	if got := w.Samples(); got != 6 {
		t.Fatalf("samples = %d, want 6", got)
	}
	if got := w.Order(); len(got) != 2 || got[0] != "a" {
		t.Fatalf("order = %v, want the source's reported order", got)
	}
}

func TestSampleWindowReportsNoSpanForAJointItNeverSaw(t *testing.T) {
	w := NewSampleWindow()
	w.Add(JointReading{Positions: map[string]float64{"a": 1}})
	if _, ok := w.Span("b"); ok {
		t.Fatal("a joint the source never reported must have no span, not a zero one")
	}
	if got := w.DirectionSign("b"); got != 0 {
		t.Fatalf("direction = %d, want 0", got)
	}
}

func TestUnsupportedBackendErrorSaysWhatIsAvailable(t *testing.T) {
	err := &UnsupportedBackendError{
		Backend:   "unitree-lowstate",
		Available: []string{"ros2-joint-states"},
		Detail:    "the robot publishes a vendor message",
	}
	msg := err.Error()
	for _, want := range []string{"unitree-lowstate", "ros2-joint-states", "vendor message"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}
