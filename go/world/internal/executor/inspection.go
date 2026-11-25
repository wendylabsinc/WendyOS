package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type Capture interface {
	Capture(context.Context) (livecapture.Snapshot, error)
}
type evidenceWriter interface {
	StoreEvidence(context.Context, sensing.EvidenceRef, []byte, time.Time) error
}

func settled(s State, spec tasking.Spec) bool {
	distance, speed := 0.0, 0.0
	for j := range s.Position {
		distance = math.Hypot(distance, s.Position[j]-spec.Target[j])
		speed = math.Hypot(speed, s.Velocity[j])
	}
	return distance <= spec.ToleranceM && speed <= spec.StopSpeedMPS
}
func (c *Controller) current(ctx context.Context) (State, error) {
	s, err := c.Adapter.State(ctx)
	if err != nil {
		return s, err
	}
	if err = c.validateState(s); err != nil {
		return s, err
	}
	if s.BootID != c.boot || s.Session != c.session {
		return s, errors.New("capture actuator session changed")
	}
	age := time.Now().UnixMilli() - s.SampleUnixMS
	if age < 0 || age > 250 {
		return s, errors.New("capture actuator state is stale")
	}
	return s, nil
}
func telemetry(s State) tasking.Telemetry {
	return tasking.Telemetry{BootID: s.BootID, Session: s.Session, Frame: s.Frame, SampleUnixMS: s.SampleUnixMS, Position: s.Position, Velocity: s.Velocity, ObstacleClear: s.ObstacleClear, Energy: s.Energy, Sensors: s.Sensors}
}

// Inspect runs only after Tick has braked at the measured target. It never
// renews motion, and checks task policy again after media has reached the node.
// The independent actuator watchdog remains effective throughout transfer.
func (c *Controller) Inspect(ctx context.Context) (tasking.Decision, error) {
	pending := c.pending
	c.pending = nil
	if pending == nil || c.Capture == nil {
		return tasking.Decision{}, errors.New("no pending inspection")
	}
	writer, ok := c.Gateway.(evidenceWriter)
	if !ok {
		return tasking.Decision{}, errors.New("inspection evidence writer unavailable")
	}
	s, err := c.current(ctx)
	if err != nil {
		return tasking.Decision{}, err
	}
	if err := validateControlState(s, pending.Spec); err != nil {
		return tasking.Decision{}, err
	}
	d, err := c.Gateway.Evaluate(ctx, pending.Spec.ID, c.epoch, telemetry(s))
	if err != nil || d.Action != mission.Continue {
		return d, err
	}
	expected, expectedErr := json.Marshal(pending.Spec)
	actual, actualErr := json.Marshal(d.Record.Spec)
	if expectedErr != nil || actualErr != nil || !bytes.Equal(expected, actual) {
		return d, errors.New("inspection decision changed the immutable task")
	}
	if d.Record.Spec.ID != pending.Spec.ID || d.Record.Status.Phase != mission.Running || d.Record.Spec.Type != tasking.InspectType || !settled(s, d.Record.Spec) {
		return d, errors.New("inspection no longer settled and running")
	}
	if err := c.Brake(ctx); err != nil {
		return d, err
	}
	settledSince := time.Now().UnixMilli()
	afterBrake, err := c.current(ctx)
	if err != nil {
		return d, err
	}
	if !settled(afterBrake, d.Record.Spec) {
		return d, errors.New("robot not settled after inspection brake")
	}
	settledSimulationNS := int64(math.Round(afterBrake.SimulationTimeS * 1e9))
	var snap livecapture.Snapshot
	for {
		snap, err = c.Capture.Capture(ctx)
		if err != nil {
			return d, err
		}
		if snap.BootID != c.boot {
			return d, errors.New("capture boot changed while waiting for settled interval")
		}
		if _, err := c.current(ctx); err != nil {
			return d, err
		}
		interval := time.Duration(0)
		for _, sample := range snap.Observations {
			if sample.Audio != nil && time.Duration(sample.Audio.DurationNS) > interval {
				interval = time.Duration(sample.Audio.DurationNS)
			}
		}
		if snap.State.Session == c.session && snap.SampleUnixMS >= settledSince+(interval+time.Millisecond-1).Milliseconds() && snap.SimulationTimeNS >= settledSimulationNS+int64(interval) {
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return d, ctx.Err()
		case <-timer.C:
		}
	}
	if snap.BootID != c.boot || snap.State.BootID != c.boot || snap.State.Session != c.session || snap.State.Frame != d.Record.Spec.Frame || !snap.State.Synthetic {
		return d, errors.New("inspection capture scope changed")
	}
	if bound := d.Record.Spec.ControlContext; bound != nil {
		if snap.RunID != bound.Context.RunID || snap.State.Provenance.PackageSHA256 != bound.Context.PackageSHA256 {
			return d, errors.New("inspection capture differs from commissioned run or package")
		}
	}
	captured := State{Position: snap.State.Position, Velocity: snap.State.Velocity}
	if !settled(captured, d.Record.Spec) {
		return d, errors.New("inspection capture is not settled at target")
	}
	retainUntil := time.UnixMilli(snap.SampleUnixMS).Add(7 * 24 * time.Hour)
	for _, sample := range snap.Observations {
		if sample.Evidence != nil {
			data, err := snap.Evidence(*sample.Evidence)
			if err != nil {
				return d, err
			}
			if err = writer.StoreEvidence(ctx, *sample.Evidence, data, retainUntil); err != nil {
				return d, err
			}
		}
	}
	s, err = c.current(ctx)
	if err != nil {
		return d, err
	}
	if err := validateControlState(s, pending.Spec); err != nil {
		return d, err
	}
	t := telemetry(s)
	t.Inspection = &tasking.Inspection{TaskID: d.Record.Spec.ID, Trigger: d.Record.Spec.Trigger, RunID: snap.RunID, BootID: snap.BootID, Session: snap.State.Session, Sequence: snap.Sequence, CapturedUnixMS: snap.SampleUnixMS, SettledSinceUnixMS: settledSince, SettledSinceSimulationNS: settledSimulationNS, SimulationTimeNS: snap.SimulationTimeNS, Frame: snap.State.Frame, Position: snap.State.Position, Velocity: snap.State.Velocity, Sensors: snap.State.Sensors, ObstacleClear: snap.State.ObstacleClear, Energy: snap.State.Energy, Samples: snap.Observations}
	return c.Gateway.Evaluate(ctx, pending.Spec.ID, c.epoch, t)
}

// Candidates remain rolling until the task store holds the selected result.
// Retries keep the capture-derived deadline. Existing bytes may be owned by a
// live publisher; verified reuse preserves that owner's retention and metadata.
func (n NodeGateway) StoreEvidence(ctx context.Context, ref sensing.EvidenceRef, data []byte, retainUntil time.Time) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return errors.New("inspection evidence bytes differ from reference")
	}
	if retainUntil.IsZero() || !time.Now().Before(retainUntil) {
		return errors.New("inspection evidence retention expired")
	}
	verifyExisting := func() error {
		got, meta, err := n.Client.DownloadEvidence(ctx, ref.SHA256, ref.Size)
		if err != nil {
			return err
		}
		if meta.Size != ref.Size || meta.ContentType != ref.ContentType || !bytes.Equal(got, data) {
			return errors.New("existing inspection evidence differs")
		}
		return nil
	}
	if _, err := n.Client.UploadRollingEvidence(ctx, data, ref.ContentType, retainUntil); err != nil {
		var status *client.HTTPStatusError
		if errors.As(err, &status) && (status.StatusCode == 401 || status.StatusCode == 403) {
			return err
		}
		// Another capture may already own these bytes or win allocation. Its
		// existing expiry may differ from this capture's requested deadline.
		if verified := verifyExisting(); verified == nil {
			return nil
		}
		return err
	}
	return verifyExisting()
}
