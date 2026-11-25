package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type captureFunc func(context.Context) (livecapture.Snapshot, error)

func (f captureFunc) Capture(ctx context.Context) (livecapture.Snapshot, error) { return f(ctx) }

type inspectionGateway struct {
	fakeGateway
	stored    int
	retention []time.Time
	fail      bool
	result    *tasking.Inspection
}

func (g *inspectionGateway) StoreEvidence(_ context.Context, _ sensing.EvidenceRef, _ []byte, retainUntil time.Time) error {
	if g.fail {
		return errors.New("unavailable evidence")
	}
	g.stored++
	g.retention = append(g.retention, retainUntil)
	return nil
}
func (g *inspectionGateway) Evaluate(ctx context.Context, id string, epoch uint64, t tasking.Telemetry) (tasking.Decision, error) {
	g.evaluations++
	if t.Inspection != nil {
		g.result = t.Inspection
	}
	return g.decision, g.err
}
func inspectController(t *testing.T) (*Controller, *fakeAdapter, *inspectionGateway) {
	t.Helper()
	c, a, base := controller(t)
	base.records[0].Spec.Type = tasking.InspectType
	base.decision.Record.Spec.Type = tasking.InspectType
	a.state.Position = base.records[0].Spec.Target
	g := &inspectionGateway{fakeGateway: *base}
	c.Gateway = g
	c.Capture = captureFunc(func(context.Context) (livecapture.Snapshot, error) {
		if a.stops == 0 || a.commands != 0 {
			t.Fatal("capture before braking or while renewing motion")
		}
		data := []byte("test media")
		sum := sha256.Sum256(data)
		ref := sensing.EvidenceRef{SHA256: hex.EncodeToString(sum[:]), ContentType: "image/png", Size: int64(len(data))}
		return livecapture.Snapshot{RunID: "run", BootID: a.state.BootID, Sequence: 1, SampleUnixMS: time.Now().UnixMilli(), State: livecapture.State{BootID: a.state.BootID, Session: a.state.Session, Frame: a.state.Frame, Position: a.state.Position, Synthetic: true}, Observations: []sensing.Sample{{Evidence: &ref}}, Media: []livecapture.Media{{SHA256: ref.SHA256, Data: data}}}, nil
	})
	return c, a, g
}
func TestInspectionBrakesAndStoresBeforeSubmittingResult(t *testing.T) {
	c, a, g := inspectController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.pending == nil || a.stops != 1 || a.commands != 0 || g.result != nil {
		t.Fatal("wrong capture scheduling")
	}
	if _, err := c.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.stored != 1 || g.result == nil || g.result.TaskID != "one" || a.commands != 0 {
		t.Fatal("result did not follow evidence transfer")
	}
	if len(g.retention) != 1 || !g.retention[0].Equal(time.UnixMilli(g.result.CapturedUnixMS).Add(7*24*time.Hour)) {
		t.Fatal("inspection retention did not preserve capture time")
	}
}
func TestInspectionFailureAndStopNeverSubmitResult(t *testing.T) {
	for _, mode := range []string{"media", "stopped", "boot", "capture failure", "out of target"} {
		t.Run(mode, func(t *testing.T) {
			c, a, g := inspectController(t)
			if _, err := c.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "media":
				g.fail = true
			case "stopped":
				g.decision.Action = mission.Stop
				g.decision.Record.Status.Phase = mission.Canceled
			case "boot":
				a.state.BootID = "new"
			case "capture failure":
				c.Capture = captureFunc(func(context.Context) (livecapture.Snapshot, error) {
					return livecapture.Snapshot{}, errors.New("stale")
				})
			case "out of target":
				a.state.Position[0] -= 1
			}
			c.Inspect(context.Background())
			if g.result != nil || a.commands != 0 {
				t.Fatal("failed capture submitted result or moved")
			}
		})
	}
}

func TestInspectionRefusesChangedPolicyBeforeCapture(t *testing.T) {
	c, a, g := inspectController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.decision.Record.Spec.MinimumEnergy = .9
	captured := false
	c.Capture = captureFunc(func(context.Context) (livecapture.Snapshot, error) {
		captured = true
		return livecapture.Snapshot{}, nil
	})
	if _, err := c.Inspect(context.Background()); err == nil {
		t.Fatal("changed immutable policy accepted")
	}
	if captured || g.stored != 0 || g.result != nil || a.commands != 0 {
		t.Fatal("changed policy reached capture, storage or motion")
	}
}

func TestInspectionWaitsForEntireAudioWindowAfterBrake(t *testing.T) {
	c, a, g := inspectController(t)
	base := c.Capture
	calls := 0
	c.Capture = captureFunc(func(ctx context.Context) (livecapture.Snapshot, error) {
		snap, err := base.Capture(ctx)
		calls++
		snap.Observations = append(snap.Observations, sensing.Sample{Audio: &sensing.Audio{DurationNS: int64(30 * time.Millisecond)}})
		snap.SimulationTimeNS = int64(30 * time.Millisecond)
		if calls == 1 {
			snap.SampleUnixMS = time.Now().Add(-time.Second).UnixMilli()
		}
		return snap, err
	})
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Inspect(ctx); err != nil {
		t.Fatal(err)
	}
	if calls < 2 || a.commands != 0 || g.result == nil || g.result.CapturedUnixMS-g.result.SettledSinceUnixMS < 30 {
		t.Fatal("pre-brake audio window accepted")
	}
}

func TestInspectionWaitsForSlowSimulationAudioWindow(t *testing.T) {
	c, a, g := inspectController(t)
	a.state.SimulationTimeS = 1
	base := c.Capture
	calls := 0
	c.Capture = captureFunc(func(ctx context.Context) (livecapture.Snapshot, error) {
		snap, err := base.Capture(ctx)
		calls++
		snap.Observations = append(snap.Observations, sensing.Sample{Audio: &sensing.Audio{DurationNS: int64(30 * time.Millisecond)}})
		// Wall time advances normally, but physics remains at the brake time for
		// several captures. Success must wait for the simulation interval too.
		snap.SimulationTimeNS = int64(time.Second)
		if calls >= 5 {
			snap.SimulationTimeNS += int64(30 * time.Millisecond)
		}
		return snap, err
	})
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Inspect(ctx); err != nil {
		t.Fatal(err)
	}
	if calls < 5 || a.commands != 0 || g.result == nil || g.result.SettledSinceSimulationNS != int64(time.Second) || g.result.SimulationTimeNS-g.result.SettledSinceSimulationNS < int64(30*time.Millisecond) {
		t.Fatal("pre-brake simulation audio window accepted")
	}
}
