package go2executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

func TestFaultSnapshotFreezesExactMeasuredMotionBeforeStop(t *testing.T) {
	c, _, a, _ := fixture(t)
	if _, ok := c.FaultSnapshot(); ok {
		t.Fatal("fault existed before execution")
	}
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	firstCommand := a.commands[0]
	a.state.BodyVelocity = [3]float64{-.04, 0, -.397}
	a.state.Scan.Ranges[180] = .4 // occupied only in the expanded measured stopping envelope
	if _, e := c.Tick(context.Background()); !errors.Is(e, go2nav.ErrBlocked) {
		t.Fatalf("expected occupied measured stopping envelope refusal, got %v", e)
	}
	s, ok := c.FaultSnapshot()
	if !ok || s.Stage != "measured-motion" || s.State.BodyVelocity != a.state.BodyVelocity || s.LastAcknowledged == nil || *s.LastAcknowledged != firstCommand || s.LastAttempt == nil || *s.LastAttempt != firstCommand || s.PreviousCommand[2] != firstCommand.YawRate || s.ExecutorEpoch != c.Generation() || !s.Complete || !s.Simulation || s.OperationalAuthority {
		t.Fatalf("fault lost pre-stop context: %+v", s)
	}
	if len(a.commands) != 1 || a.stops == 0 {
		t.Fatal("diagnostic changed motion refusal")
	}
	originalRange := s.State.Scan.Ranges[0]
	originalPoint := s.Route[0]
	originalDigest := s.Spec.Go2.PackageSHA256
	s.State.Scan.Ranges[0] = -1
	s.Route[0].X = 99
	s.LastAttempt.BodyVX = 99
	s.Spec.Go2.PackageSHA256 = "mutated"
	a.state.Scan.Ranges[0] = -2
	s2, _ := c.FaultSnapshot()
	if s2.State.Scan.Ranges[0] != originalRange || s2.Route[0] != originalPoint || *s2.LastAttempt != firstCommand || s2.Spec.Go2.PackageSHA256 != originalDigest {
		t.Fatal("diagnostic aliases caller or adapter state")
	}
	b, e := json.Marshal(s2)
	if e != nil {
		t.Fatal(e)
	}
	if len(b) > MaxFaultJSONBytes || bytes.Contains(b, []byte("private_key")) || bytes.Contains(b, []byte(`"proof"`)) {
		t.Fatal("unbounded or credential-bearing snapshot")
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(b, &fields); e != nil {
		t.Fatal(e)
	}
	decoded, e := DecodeState(fields["state"])
	if e != nil {
		t.Fatal(e)
	}
	if decoded.Scan.Ranges[0] != originalRange || decoded.BodyVelocity != s2.State.BodyVelocity || decoded.Scan.SensorMount != s2.State.Scan.SensorMount {
		t.Fatal("raw finite observations changed through diagnostic codec")
	}
	_, _ = c.Tick(context.Background())
	s3, _ := c.FaultSnapshot()
	after, _ := json.Marshal(s3)
	if !bytes.Equal(b, after) {
		t.Fatal("later reporting changed first fault")
	}
}

func TestFaultCodecUnknownRaysAndInvalidNumbersAreNull(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	a.stateHook = func(s *State) { s.Scan.Ranges[0] = math.Inf(1); s.Scan.Ranges[1] = math.NaN(); s.Pose.X = math.NaN() }
	if _, e := c.Tick(context.Background()); e == nil {
		t.Fatal("invalid state accepted")
	}
	s, ok := c.FaultSnapshot()
	if !ok {
		t.Fatal("missing invalid-state snapshot")
	}
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	state := out["state"].(map[string]any)
	scan := state["scan"].(map[string]any)
	ranges := scan["ranges"].([]any)
	if ranges[0] != nil || ranges[1] != nil || state["pose"].(map[string]any)["x"] != nil || ranges[2] != float64(4) {
		t.Fatal("missing/nonfinite data invented numbers or changed finite rays")
	}
}

func TestFaultSnapshotBoundsAndDispatchOutcome(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	a.commandErr = errors.New(strings.Repeat("x", 600))
	if _, e := c.Tick(context.Background()); e == nil {
		t.Fatal("uncertain dispatch accepted")
	}
	s, ok := c.FaultSnapshot()
	if !ok || s.Stage != "dispatch" || s.LastAttempt == nil || s.LastAcknowledged != nil || len(s.Failure) != 512 || s.Complete {
		t.Fatal("uncertain dispatch mislabeled as acknowledged or unlimited")
	}
	if _, e := json.Marshal(s); e != nil {
		t.Fatal(e)
	}
	s.State.Scan.Ranges = make([]float64, 8193)
	if _, e := json.Marshal(s); e == nil {
		t.Fatal("caller-expanded scan bypassed serialization bound")
	}
	s.State.Scan.Ranges = nil
	s.Route = make([]go2nav.Point, 257)
	if _, e := json.Marshal(s); e == nil {
		t.Fatal("caller-expanded route bypassed serialization bound")
	}
}

func TestFaultSnapshotRetainsRejectedStateNotPreviousValidState(t *testing.T) {
	c, _, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	a.stateHook = func(s *State) { s.ScanPublisher = "changed-sensor" }
	if _, e := c.Tick(context.Background()); e == nil {
		t.Fatal("publisher change accepted")
	}
	s, ok := c.FaultSnapshot()
	if !ok || s.Stage != "initial-state" || s.State.ScanPublisher != "changed-sensor" {
		t.Fatal("failure snapshot replaced invalid observation with old valid data")
	}
}
