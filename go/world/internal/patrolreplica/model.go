// Package patrolreplica hands retained simulated patrol results to read-only
// peers. A report is a publisher's account of executor files, not task authority.
package patrolreplica

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const LiveType = "wendy.nav.patrol.remote-progress.v1"
const ReportType = "wendy.nav.patrol.debrief.v1"
const MaxReports = 17 // Sixteen captures plus one terminal result; Query stays below 1 MiB.
const MaxSourceBytes = 16 << 20

var ExecutorSource = world.Source{Device: "go2", App: "world-patrol", Service: "patrol", Stream: "progress"}

type Config struct {
	Tenant        string         `json:"tenant"`
	Site          string         `json:"site"`
	RunID         string         `json:"run_id"`
	MissionID     string         `json:"mission_id"`
	SpecSHA256    string         `json:"spec_sha256"`
	Frame         world.FrameRef `json:"frame"`
	Source        world.Source   `json:"source"`
	RetainUntilMS int64          `json:"retain_until_unix_ms"`
}

func (c Config) Validate() error {
	if c.Tenant != "simulation" || c.Site != "studio-01" || len(c.RunID) < 1 || len(c.RunID) > 128 || c.MissionID == "" || len(c.MissionID) > 256 || !sensing.DigestValid(c.SpecSHA256) || c.Frame.Validate() != nil || c.Source.Validate() != nil || c.Source == ExecutorSource || c.RetainUntilMS <= 0 {
		return errors.New("invalid simulated patrol debrief scope")
	}
	return nil
}
func (c Config) Clock() string { return "simulation:" + c.RunID }
func Hash(b []byte) string     { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func JSONHash(v any) string    { b, _ := json.Marshal(v); return Hash(b) }
func Decode(b []byte, v any, max int) error {
	if len(b) > max {
		return errors.New("JSON exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

type Snapshot struct {
	Patrol     patrol.State      `json:"patrol"`
	Task       tasking.Record    `json:"task"`
	RunID      string            `json:"run_id"`
	Components []world.Component `json:"components"`
}

// DecodeSnapshot admits only the explicitly bound mission and original executor
// progress. Unrelated status fields are intentionally not copied to reports.
func DecodeSnapshot(b []byte, c Config, now time.Time) (Snapshot, world.Component, error) {
	var s Snapshot
	if len(b) > 1<<20 {
		return s, world.Component{}, errors.New("status exceeds bound")
	}
	if e := json.Unmarshal(b, &s); e != nil {
		return s, world.Component{}, e
	}
	if s.RunID != c.RunID || s.Patrol.MissionID != c.MissionID || s.Task.Spec.ID != c.MissionID || s.Task.Spec.Site != c.Site || s.Task.Spec.Frame != c.Frame || s.Task.Spec.Patrol == nil || s.Task.Spec.Type != tasking.PatrolType || JSONHash(s.Task.Spec) != c.SpecSHA256 || s.Patrol.Schema != patrol.StatusType || len(s.Patrol.Rooms) < 1 || len(s.Patrol.Rooms) > 32 {
		return s, world.Component{}, errors.New("executor mission scope changed")
	}
	for _, v := range s.Components {
		if v.Key.Type != patrol.StatusType {
			continue
		}
		var state patrol.State
		if v.Key.Tenant != c.Tenant || v.Key.Site != c.Site || v.Key.Entity != c.MissionID || v.Key.Source != ExecutorSource || v.Frame != c.Frame || v.Version.Epoch != 1 || v.Version.Sequence == 0 || v.Tombstone || v.TTL != 5*time.Second || v.Timing.ClockDomain != "simulation-executor-wall:"+c.RunID || !v.Timing.ErrorKnown || v.Timing.ErrorBound != 0 || v.Timing.AgeHint != 0 || v.Timing.SampleTime.IsZero() || v.Timing.SampleTime.After(now) || v.Provenance.Origin != world.Simulated || v.Provenance.Simulation == nil || v.Provenance.Simulation.RunID != c.RunID || v.Provenance.Validate() != nil || json.Unmarshal(v.Payload, &state) != nil || state.MissionID != c.MissionID || state.Schema != patrol.StatusType || len(state.Rooms) < 1 || len(state.Rooms) > 32 {
			return s, v, errors.New("invalid original executor progress")
		}
		s.Patrol = state
		return s, v, nil
	}
	return s, world.Component{}, errors.New("missing original executor progress")
}

type Evidence struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SensorID    string `json:"sensor_id"`
}
type Report struct {
	ContentSHA256  string            `json:"content_sha256"`
	Schema         string            `json:"schema"`
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	RunID          string            `json:"run_id"`
	MissionID      string            `json:"mission_id"`
	SpecSHA256     string            `json:"spec_sha256"`
	OriginalUnixMS int64             `json:"original_unix_ms"`
	OriginalClock  string            `json:"original_clock"`
	SimulationNS   int64             `json:"simulation_ns,omitempty"`
	Sequence       uint64            `json:"sequence,omitempty"`
	Frame          world.FrameRef    `json:"frame"`
	RetainUntilMS  int64             `json:"retain_until_unix_ms"`
	State          *patrol.State     `json:"state,omitempty"`
	Task           *tasking.Record   `json:"task,omitempty"`
	RoomIDs        []string          `json:"room_ids,omitempty"`
	Evidence       []Evidence        `json:"evidence"`
	Samples        []json.RawMessage `json:"samples,omitempty"`
}

func (r Report) Identity() string {
	return JSONHash(struct {
		Kind, RunID, MissionID, SpecSHA256 string
		Sequence                           uint64
	}{r.Kind, r.RunID, r.MissionID, r.SpecSHA256, r.Sequence})
}
func (r Report) ContentDigest() string { r.ContentSHA256 = ""; return JSONHash(r) }
func Terminal(s string) bool {
	return s == "completed" || s == "failed" || s == "stopped" || s == "expired"
}
func (r Report) Validate(c Config) error {
	if r.Schema != ReportType || r.ID != r.Identity() || r.ContentSHA256 != r.ContentDigest() || r.RunID != c.RunID || r.MissionID != c.MissionID || r.SpecSHA256 != c.SpecSHA256 || r.Frame != c.Frame || r.RetainUntilMS != c.RetainUntilMS || r.OriginalUnixMS <= 0 || r.OriginalUnixMS >= r.RetainUntilMS || r.OriginalClock != "simulation-executor-wall:"+c.RunID || len(r.Evidence) > 8 {
		return errors.New("invalid historical report scope or content identity")
	}
	seen := map[string]bool{}
	for _, e := range r.Evidence {
		if !sensing.DigestValid(e.SHA256) || seen[e.SHA256] || e.Size < 1 || e.Size > MaxSourceBytes || (e.ContentType != "application/json" && e.ContentType != "image/png" && e.ContentType != "image/jpeg" && e.ContentType != "audio/wav") {
			return errors.New("invalid report evidence")
		}
		seen[e.SHA256] = true
	}
	if r.Kind == "capture" {
		if r.State != nil || r.Task != nil || len(r.Samples) != 5 || r.Sequence == 0 || r.SimulationNS <= 0 || len(r.RoomIDs) < 1 || len(r.RoomIDs) > 32 {
			return errors.New("invalid capture report")
		}
		required := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio", "lidar-go2": "lidar"}
		for _, raw := range r.Samples {
			s, e := sensing.Decode(raw)
			if e != nil || s.RunID != c.RunID || s.SiteID != c.Site || s.Frame != c.Frame || s.Sequence != r.Sequence || s.SampleNS > r.SimulationNS || r.SimulationNS-s.SampleNS > 150000000 {
				return errors.New("invalid capture sample provenance")
			}
			if required[s.SensorID] != s.Kind {
				return errors.New("duplicate or unknown historical sensor")
			}
			delete(required, s.SensorID)
			if s.Evidence != nil {
				matched := false
				for _, ref := range r.Evidence {
					if ref.SHA256 == s.Evidence.SHA256 && ref.Size == s.Evidence.Size && ref.ContentType == s.Evidence.ContentType && ref.SensorID == s.SensorID {
						matched = true
					}
				}
				if !matched {
					return errors.New("capture reference metadata absent or changed")
				}
			}
		}
		if len(required) != 0 || len(r.Evidence) != 4 {
			return errors.New("incomplete original sensor set")
		}
		manifest, _ := json.Marshal(map[string]any{"schema": "wendy.nav.patrol.capture.v1", "run_id": c.RunID, "captured_unix_ms": r.OriginalUnixMS, "samples": r.Samples})
		matched := false
		for _, ref := range r.Evidence {
			if ref.SensorID == "manifest" && ref.SHA256 == Hash(manifest) && ref.Size == int64(len(manifest)) && ref.ContentType == "application/json" {
				matched = true
			}
		}
		if !matched {
			return errors.New("original manifest reference changed")
		}
		rooms := map[string]bool{}
		for _, id := range r.RoomIDs {
			if id == "" || len(id) > 128 || rooms[id] {
				return errors.New("invalid original room attribution")
			}
			rooms[id] = true
		}
		return nil
	}
	if r.Kind == "result" && r.State != nil && r.Task != nil {
		expected := mission.Failed
		if r.State.State == "completed" {
			expected = mission.Succeeded
			if r.Task.PatrolReturnResult == nil {
				return errors.New("completed report lacks retained arrival evidence")
			}
		} else if r.State.State == "stopped" {
			expected = mission.Canceled
		}
		if r.Task.Status.Phase != expected {
			return errors.New("terminal task and patrol outcome disagree")
		}
	}
	if r.Kind != "result" || r.State == nil || r.Task == nil || !Terminal(r.State.State) || !r.Task.Status.Phase.Terminal() || r.State.MissionID != c.MissionID || JSONHash(r.Task.Spec) != c.SpecSHA256 || r.Sequence != 0 || len(r.Samples) > 0 || len(r.Evidence) != 0 {
		return errors.New("invalid terminal historical report")
	}
	return nil
}
func baseReport(c Config, kind string, ms int64) Report {
	return Report{Schema: ReportType, Kind: kind, RunID: c.RunID, MissionID: c.MissionID, SpecSHA256: c.SpecSHA256, Frame: c.Frame, RetainUntilMS: c.RetainUntilMS, OriginalUnixMS: ms, OriginalClock: "simulation-executor-wall:" + c.RunID, Evidence: []Evidence{}}
}
func Result(c Config, s Snapshot, v world.Component) (Report, error) {
	r := baseReport(c, "result", v.Timing.SampleTime.UnixMilli())
	r.State = &s.Patrol
	r.Task = &s.Task
	r.ID = r.Identity()
	r.ContentSHA256 = r.ContentDigest()
	return r, r.Validate(c)
}

// Capture validates the complete local atomic capture before any upload. Room
// attribution requires the executor's existing coverage reference to its exact
// manifest; file presence alone does not establish a completed room visit.
func Capture(c Config, s Snapshot, b []byte) (Report, map[string][]byte, error) {
	var raw struct {
		Samples    []json.RawMessage `json:"samples"`
		Media      map[string][]byte `json:"media"`
		TimeS      float64           `json:"simulation_time_s"`
		CapturedMS int64             `json:"captured_unix_ms"`
	}
	if e := Decode(b, &raw, MaxSourceBytes); e != nil {
		return Report{}, nil, e
	}
	r := baseReport(c, "capture", raw.CapturedMS)
	manifest, _ := json.Marshal(map[string]any{"schema": "wendy.nav.patrol.capture.v1", "run_id": c.RunID, "captured_unix_ms": raw.CapturedMS, "samples": raw.Samples})
	mh := Hash(manifest)
	for _, room := range s.Patrol.Rooms {
		for _, ref := range room.Evidence {
			if ref == mh {
				r.RoomIDs = append(r.RoomIDs, room.ID)
				break
			}
		}
	}
	sort.Strings(r.RoomIDs)
	blobs := map[string][]byte{mh: manifest}
	r.Evidence = append(r.Evidence, Evidence{mh, int64(len(manifest)), "application/json", "manifest"})
	required := map[string]string{"camera-a": "camera", "camera-b": "camera", "gas-1": "gas", "audio-1": "audio", "lidar-go2": "lidar"}
	for _, rawSample := range raw.Samples {
		sample, e := sensing.Decode(rawSample)
		if e != nil {
			return r, nil, e
		}
		if required[sample.SensorID] != sample.Kind {
			return r, nil, errors.New("duplicate or unexpected capture sensor")
		}
		delete(required, sample.SensorID)
		if r.Sequence == 0 {
			r.Sequence = sample.Sequence
		}
		r.SimulationNS = max(r.SimulationNS, sample.SampleNS)
		if sample.Evidence != nil {
			ref := sample.Evidence
			data, ok := raw.Media[ref.SHA256]
			if !ok || int64(len(data)) != ref.Size || Hash(data) != ref.SHA256 {
				return r, nil, errors.New("capture original missing or digest mismatch")
			}
			blobs[ref.SHA256] = data
			r.Evidence = append(r.Evidence, Evidence{ref.SHA256, ref.Size, ref.ContentType, sample.SensorID})
		}
	}
	if len(required) != 0 || len(blobs) != len(raw.Media)+1 {
		return r, nil, errors.New("incomplete capture or unreferenced media")
	}
	r.Samples = raw.Samples
	r.ID = r.Identity()
	r.ContentSHA256 = r.ContentDigest()
	return r, blobs, r.Validate(c)
}
func Component(c Config, r Report, epoch, seq uint64, now time.Time) (world.Component, error) {
	if e := r.Validate(c); e != nil {
		return world.Component{}, e
	}
	if r.OriginalUnixMS > now.UnixMilli() {
		return world.Component{}, errors.New("original report time is in the future")
	}
	ttl := time.UnixMilli(r.RetainUntilMS).Sub(now)
	if ttl < time.Millisecond {
		return world.Component{}, errors.New("report retention elapsed")
	}
	original := time.UnixMilli(r.OriginalUnixMS)
	payload, _ := json.Marshal(r)
	return world.Component{Key: world.Key{Tenant: c.Tenant, Site: c.Site, Entity: "patrol-report/" + r.ID, Type: ReportType, Source: c.Source}, Version: world.Version{Epoch: epoch, Sequence: seq}, Frame: c.Frame, Timing: world.Timing{SampleTime: now, ClockDomain: c.Clock(), ErrorKnown: true}, TTL: ttl, Provenance: world.Provenance{Origin: world.Replayed, AppVersion: "world-patrol-replica-v1", OriginalCaptureTime: &original, OriginalClockDomain: r.OriginalClock, Simulation: &world.SimulationContext{RunID: c.RunID, ScenarioVersion: "go2-passive-patrol-v1", Seed: 7, ClockDomain: c.Clock()}}, Payload: payload}, nil
}
func Live(c Config, s Snapshot, v world.Component, epoch, seq uint64) world.Component {
	b, _ := json.Marshal(struct {
		RunID           string        `json:"run_id"`
		SpecSHA256      string        `json:"spec_sha256"`
		OriginalSource  world.Source  `json:"original_source"`
		OriginalVersion world.Version `json:"original_version"`
		State           patrol.State  `json:"state"`
	}{c.RunID, c.SpecSHA256, ExecutorSource, v.Version, s.Patrol})
	v.Key = world.Key{Tenant: c.Tenant, Site: c.Site, Entity: c.MissionID, Type: LiveType, Source: c.Source}
	v.Version = world.Version{Epoch: epoch, Sequence: seq}
	v.Payload = b
	v.Timing.ClockDomain = c.Clock()
	simulation := *v.Provenance.Simulation
	v.Provenance.Simulation = &simulation
	v.Provenance.Simulation.ClockDomain = c.Clock()
	v.Provenance.AppVersion = "world-patrol-replica-v1"
	return v
}
func Describe(c Config) string { return fmt.Sprintf("%s / %s / %s", c.Site, c.RunID, c.MissionID) }
