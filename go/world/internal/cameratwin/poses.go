package cameratwin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PublishPose records accepted calibration and invalidation in the same durable
// local World replica as person observations. Consumers still gate it with the
// current source session; persistence never makes a restarted camera located.
func (s *Store) PublishPose(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.lookup(id)
	if !ok || id == "" || len(id) > 64 {
		return errors.New("unknown camera")
	}
	// Read the current controller state rather than accepting a caller snapshot.
	// A racing solve or disconnect must not publish a superseded pose.
	state := c.Pose
	if state.Site.ID != s.site.ID || state.Site.ModelRevision != s.site.ModelRevision || state.Site.ModelSHA256 != s.site.ModelSHA256 {
		return nil
	}
	current := state.Fresh && state.Status == "located" && state.Pose != nil && state.CoordinateFrame == s.frame.ID && c.State.Fresh && c.State.Latest != nil && state.SourceSession == c.State.Latest.SourceSession && state.Pose.ValidUntil.After(s.now())
	if !current && s.poseDigests[id] == "" {
		return nil
	}
	if len(s.poseDigests) >= 16 && s.poseDigests[id] == "" {
		return errors.New("camera limit reached")
	}
	now := s.now()
	component := world.Component{Key: world.Key{Tenant: "local-camera-demo", Site: s.site.ID, Entity: "camera:" + id, Type: "wendy.camera-pose.state.v1", Source: s.source}, Version: world.Version{Epoch: s.epoch, Sequence: s.sequence + 1}, Frame: s.frame, Timing: world.Timing{SampleTime: now, ClockDomain: "local-camera-receipt", ErrorKnown: true}, TTL: time.Second}
	digest := ""
	if current {
		if len(state.Evidence) != 2 {
			return errors.New("accepted pose needs both original receipts")
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		digest = hex.EncodeToString(sum[:])
		if digest == s.poseDigests[id] {
			return nil
		}
		component.Payload = data
		component.TTL = state.Pose.ValidUntil.Sub(now)
		component.Provenance = world.Provenance{Origin: world.Inferred, AppVersion: "world-camera-twin-v1", ModelID: "wendyos-site-localization", ModelVersion: "native-bridge-v1"}
		for _, in := range state.Evidence {
			if in.SourceSession != state.SourceSession || in.Origin != state.InputOrigin {
				return errors.New("inconsistent pose evidence")
			}
			component.Provenance.Inputs = append(component.Provenance.Inputs, s.leaf(id, in.SourceSession+":"+in.ReceiptID, in.Origin))
		}
	} else {
		component.Tombstone = true
		component.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "world-camera-twin-v1"}
	}
	if _, err := s.replica.Apply(s.source, component); err != nil {
		return err
	}
	s.sequence++
	s.poseDigests[id] = digest
	return nil
}

// Preserve the run attached to the retained camera receipt. The adapter binds
// one run to the whole history, so its latest receipt also identifies older pose
// evidence after it leaves the bounded recent window. Older fixture histories
// have no per-receipt run ID and retain their original camera-fixture context.
func (s *Store) leaf(camera, id string, origin world.Origin) world.Input {
	in := world.Input{ID: camera + ":" + id, Origin: origin}
	if origin == world.Simulated {
		c, _ := s.lookup(camera)
		runID := "camera-fixture"
		if c.State.Latest != nil && c.State.Latest.SimulationRun != "" {
			runID = c.State.Latest.SimulationRun
		}
		for _, receipt := range c.State.Recent {
			if receipt.SourceSession+":"+receipt.ID == id && receipt.SimulationRun != "" {
				runID = receipt.SimulationRun
				break
			}
		}
		identity, _ := json.Marshal([]string{"local-camera-demo", "stationary-camera", c.State.Camera.Device, c.State.Camera.ID})
		hash := sha256.Sum256(identity)
		in.Simulation = &world.SimulationContext{RunID: runID, ScenarioVersion: "stationary-camera-v1", ClockDomain: "local-receipt:" + hex.EncodeToString(hash[:])}
	}
	return in
}
