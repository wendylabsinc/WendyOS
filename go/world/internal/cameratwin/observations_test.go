package cameratwin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (*Store, *time.Time, map[string]Camera, Batch, string) {
	t.Helper()
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456789, time.UTC)
	dir := t.TempDir()
	site := camerapose.Site{ID: "room", ModelRevision: "rev1", ModelSHA256: strings.Repeat("a", 64)}
	frame := "wendy-site:room:rev1:scan-local-y-up"
	r := stationarycamera.Receipt{ID: "r1", SHA256: strings.Repeat("b", 64), SourceSession: "s1", Sequence: 1, ReceivedAt: now, RetainedUntil: now.Add(time.Hour), Origin: world.Simulated}
	p := camerapose.State{Status: "located", Site: site, CoordinateFrame: frame, Fresh: true, SourceSession: "s1", InputOrigin: world.Simulated, Pose: &camerapose.Pose{Position: [3]float64{0, 2, 0}, ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(time.Hour)}}
	cameras := map[string]Camera{"one": {State: stationarycamera.State{Fresh: true, Latest: &r, Recent: []stationarycamera.Receipt{r}}, Pose: p}}
	store, err := Open(dir, site, func(id string) (Camera, bool) { c, ok := cameras[id]; return c, ok }, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	b := Batch{Schema: Schema, CoordinateFrame: frame, ModelSHA256: site.ModelSHA256, FloorY: 0, Observations: []Observation{{ID: "anonymous", Position: [3]float64{0, 0, 2}, ObservedAt: now, ValidUntil: now.Add(Lifetime), Provenance: "inferred", InputOrigin: world.Simulated, Inputs: []Input{{CameraID: "one", ReceiptID: r.ID, SHA256: r.SHA256, SourceSession: r.SourceSession, Sequence: r.Sequence, ReceivedAt: r.ReceivedAt, Origin: r.Origin, PoseObservedAt: p.Pose.ObservedAt}}}}}
	return store, &now, cameras, b, dir
}
func clone(b Batch) Batch {
	data, _ := json.Marshal(b)
	var out Batch
	_ = json.Unmarshal(data, &out)
	return out
}
func TestDurablePublicationDuplicateExpiryAndRestart(t *testing.T) {
	s, now, cameras, b, dir := fixture(t)
	if err := s.Publish(b); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Observations) != 1 {
		t.Fatal("missing live observation")
	}
	*now = now.Add(500 * time.Millisecond)
	if err := s.Publish(b); err != nil {
		t.Fatal(err)
	}
	records, err := s.replica.Records()
	if err != nil || len(records) != 1 || records[0].Version.Sequence != 1 || records[0].Provenance.Origin != world.Inferred || records[0].Provenance.Inputs[0].Origin != world.Simulated {
		t.Fatal("missing inferred durable component", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "people-world.json")); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if len(s.Snapshot().Observations) != 0 {
		t.Fatal("expired person visible")
	}
	if s.Publish(b) == nil {
		t.Fatal("duplicate extended expired claim")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, s.site, func(id string) (Camera, bool) { c, ok := cameras[id]; return c, ok }, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.Snapshot().Observations) != 0 {
		t.Fatal("restart resurrected live claim")
	}
}
func TestChangedDependenciesRemoveClaims(t *testing.T) {
	for _, kind := range []string{"disconnect", "session", "pose", "model", "frame", "origin"} {
		t.Run(kind, func(t *testing.T) {
			s, _, cs, b, _ := fixture(t)
			defer s.Close()
			if err := s.Publish(b); err != nil {
				t.Fatal(err)
			}
			c := cs["one"]
			switch kind {
			case "disconnect":
				c.State.Fresh = false
			case "session":
				c.Pose.SourceSession = "next"
			case "pose":
				p := *c.Pose.Pose
				p.ObservedAt = p.ObservedAt.Add(time.Second)
				c.Pose.Pose = &p
			case "model":
				c.Pose.Site.ModelSHA256 = strings.Repeat("c", 64)
			case "frame":
				c.Pose.CoordinateFrame = "other"
			case "origin":
				c.Pose.InputOrigin = world.Measured
			}
			cs["one"] = c
			if len(s.Snapshot().Observations) != 0 {
				t.Fatal("changed dependency stayed current")
			}
			if s.Publish(b) == nil {
				t.Fatal("changed dependency accepted")
			}
		})
	}
}
func TestRejectForgedEvidenceLifetimeAndProjection(t *testing.T) {
	for _, kind := range []string{"digest", "receipt", "time", "expiry", "provenance", "floor", "range", "duplicate-camera", "origin"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, base, _ := fixture(t)
			defer s.Close()
			b := clone(base)
			o := &b.Observations[0]
			switch kind {
			case "digest":
				o.Inputs[0].SHA256 = strings.Repeat("d", 64)
			case "receipt":
				o.Inputs[0].ReceiptID = "forged"
			case "time":
				o.Inputs[0].ReceivedAt = o.Inputs[0].ReceivedAt.Add(time.Second)
			case "expiry":
				o.ValidUntil = o.ValidUntil.Add(time.Second)
			case "provenance":
				o.Provenance = "measured"
			case "floor":
				o.Position[1] = 1
			case "range":
				o.Position[2] = 20
			case "duplicate-camera":
				o.Inputs = append(o.Inputs, o.Inputs[0])
			case "origin":
				o.InputOrigin = world.Measured
			}
			if s.Publish(b) == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
}
func TestEmptyBatchCannotResurrectSameInputs(t *testing.T) {
	s, _, _, b, _ := fixture(t)
	defer s.Close()
	if err := s.Publish(b); err != nil {
		t.Fatal(err)
	}
	empty := clone(b)
	empty.Observations = []Observation{}
	if err := s.Publish(empty); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Observations) != 0 {
		t.Fatal("empty batch did not clear")
	}
	if s.Publish(b) == nil {
		t.Fatal("retired inputs resurrected")
	}
}
func TestClientFractionalSecondsCanonicalized(t *testing.T) {
	s, _, _, b, _ := fixture(t)
	defer s.Close()
	o := &b.Observations[0]
	o.ObservedAt = o.ObservedAt.Truncate(time.Millisecond)
	o.ValidUntil = o.ValidUntil.Truncate(time.Millisecond)
	o.Inputs[0].ReceivedAt = o.Inputs[0].ReceivedAt.Truncate(time.Millisecond)
	o.Inputs[0].PoseObservedAt = o.Inputs[0].PoseObservedAt.Truncate(time.Millisecond)
	if err := s.Publish(b); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Observations[0].ObservedAt.Nanosecond() != 123456789 {
		t.Fatal("server receipt precision lost")
	}
}

func TestPosePublicationInvalidationAndRestart(t *testing.T) {
	s, _, cs, b, _ := fixture(t)
	defer s.Close()
	c := cs["one"]
	first := b.Observations[0].Inputs[0]
	c.Pose.Evidence = []camerapose.Evidence{{ReceiptID: "calibration-a", SHA256: first.SHA256, SourceSession: first.SourceSession, Sequence: 1, ReceivedAt: first.ReceivedAt.Add(-time.Second), Origin: first.Origin}, {ReceiptID: "calibration-b", SHA256: first.SHA256, SourceSession: first.SourceSession, Sequence: 2, ReceivedAt: first.ReceivedAt, Origin: first.Origin}}
	cs["one"] = c
	if err := s.PublishPose("one"); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishPose("one"); err != nil {
		t.Fatal(err)
	}
	records, err := s.replica.Records()
	if err != nil || len(records) != 1 || records[0].Key.Type != "wendy.camera-pose.state.v1" || records[0].Version.Sequence != 1 || records[0].Provenance.Origin != world.Inferred {
		t.Fatal("pose component absent", err)
	}
	c.State.Fresh = false
	c.Pose.Fresh = false
	c.Pose.Pose = nil
	cs["one"] = c
	if err := s.PublishPose("one"); err != nil {
		t.Fatal(err)
	}
	records, err = s.replica.Records()
	if err != nil || len(records) != 1 || !records[0].Tombstone {
		t.Fatal("invalidated pose not tombstoned", err)
	}
}
func TestCrossCameraAssociationTimeBound(t *testing.T) {
	s, now, cs, b, _ := fixture(t)
	defer s.Close()
	c := cs["one"]
	r := *c.State.Latest
	r.ID = "r-two"
	r.ReceivedAt = r.ReceivedAt.Add(-400 * time.Millisecond)
	c.State.Latest = &r
	c.State.Recent = []stationarycamera.Receipt{r}
	cs["two"] = c
	in := b.Observations[0].Inputs[0]
	in.CameraID = "two"
	in.ReceiptID = r.ID
	in.ReceivedAt = r.ReceivedAt
	b.Observations[0].Inputs = append(b.Observations[0].Inputs, in)
	b.Observations[0].ValidUntil = now.Add(Lifetime - 400*time.Millisecond)
	if s.Publish(b) == nil {
		t.Fatal("noncontemporaneous cameras merged")
	}
}

func TestRestartRetiresUnexpiredPoseInDurableReplica(t *testing.T) {
	s, now, cs, b, dir := fixture(t)
	c := cs["one"]
	in := b.Observations[0].Inputs[0]
	c.Pose.Evidence = []camerapose.Evidence{{ReceiptID: "p1", SourceSession: in.SourceSession, Origin: in.Origin}, {ReceiptID: "p2", SourceSession: in.SourceSession, Origin: in.Origin}}
	cs["one"] = c
	if err := s.PublishPose("one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, s.site, func(id string) (Camera, bool) { c, ok := cs[id]; return c, ok }, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	records, err := reopened.replica.Records()
	if err != nil || len(records) != 1 || !records[0].Tombstone || records[0].Version.Epoch <= s.epoch {
		t.Fatal("unexpired pose survived restart as a current durable component", err)
	}
}
func TestFailedPersistenceDoesNotAcknowledgePublication(t *testing.T) {
	s, _, cs, b, _ := fixture(t)
	defer s.Close()
	_ = s.replica.Close()
	if s.Publish(b) == nil || len(s.Snapshot().Observations) != 0 {
		t.Fatal("failed person publication acknowledged")
	}
	c := cs["one"]
	in := b.Observations[0].Inputs[0]
	c.Pose.Evidence = []camerapose.Evidence{{ReceiptID: "p1", SourceSession: in.SourceSession, Origin: in.Origin}, {ReceiptID: "p2", SourceSession: in.SourceSession, Origin: in.Origin}}
	cs["one"] = c
	if s.PublishPose("one") == nil || s.poseDigests["one"] != "" {
		t.Fatal("failed pose publication acknowledged")
	}
}
