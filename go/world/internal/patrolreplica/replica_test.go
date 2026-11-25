package patrolreplica

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (Config, Snapshot, world.Component, []byte) {
	t.Helper()
	now := time.Now().Add(-time.Second)
	c := Config{Tenant: "simulation", Site: "studio-01", RunID: "trial", MissionID: "patrol-trial", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, Source: world.Source{Device: "go2", App: "world-patrol-replica", Service: "debrief", Stream: "reports"}, RetainUntilMS: time.Now().Add(30 * time.Minute).UnixMilli()}
	s := Snapshot{RunID: c.RunID, Patrol: patrol.State{Schema: patrol.StatusType, MissionID: c.MissionID, State: "waiting", Rooms: []patrol.Room{{ID: "lounge", Name: "Lounge", Polygon: [][2]float64{{0, 0}, {2, 0}, {2, 2}}, Visits: 1}}}, Task: tasking.Record{Spec: tasking.Spec{ID: c.MissionID, Site: c.Site, Frame: c.Frame, Type: tasking.PatrolType, Patrol: &tasking.PatrolPolicy{}}, Status: mission.Status{Phase: mission.Running}}}
	c.SpecSHA256 = JSONHash(s.Task.Spec)
	samples := []json.RawMessage{}
	media := map[string][]byte{}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1", "lidar-go2"} {
		sample := sensing.Sample{SchemaVersion: sensing.Schema, ID: c.RunID + "/" + id + "/1", SiteID: c.Site, Frame: c.Frame, SensorID: id, Sequence: 1, SampleNS: 2000000000, RunID: c.RunID, ScenarioVersion: "go2-passive-patrol-v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: id, PositionM: []float64{0, 0, 0}, CalibrationRevision: "v1"}
		switch id {
		case "camera-a", "camera-b":
			sample.Kind = "camera"
			sample.Camera = &sensing.Camera{Width: 1, Height: 1, FX: 1, FY: 1, CX: .5, CY: .5, CalibrationValidForNS: 10000000000, CameraToSite: []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}}
			sample.Evidence = &sensing.EvidenceRef{SHA256: Hash([]byte(id)), Size: int64(len(id)), ContentType: "image/png"}
		case "gas-1":
			sample.Kind = "gas"
			sample.Gas = &sensing.Gas{ValuePPM: 20, UncertaintyPPM: 1, Species: "synthetic-test-gas"}
		case "audio-1":
			sample.Kind = "audio"
			sample.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: 1000000000}
			sample.Evidence = &sensing.EvidenceRef{SHA256: Hash([]byte(id)), Size: int64(len(id)), ContentType: "audio/wav"}
		case "lidar-go2":
			sample.Kind = "lidar"
			sample.Lidar = &sensing.Lidar{PointsM: [][3]float64{}, MaxRangeM: 10}
		}
		if sample.Evidence != nil {
			media[sample.Evidence.SHA256] = []byte(id)
		}
		b, _ := json.Marshal(sample)
		if _, e := sensing.Decode(b); e != nil {
			t.Fatal(e)
		}
		samples = append(samples, b)
	}
	manifest, _ := json.Marshal(map[string]any{"schema": "wendy.nav.patrol.capture.v1", "run_id": c.RunID, "captured_unix_ms": now.UnixMilli(), "samples": samples})
	s.Patrol.Rooms[0].Evidence = []string{Hash(manifest)}
	components, e := patrol.Components(s.Patrol, c.RunID, c.Frame, 3, now)
	if e != nil {
		t.Fatal(e)
	}
	s.Components = components
	capture, _ := json.Marshal(map[string]any{"samples": samples, "media": media, "simulation_time_s": 2, "captured_unix_ms": now.UnixMilli()})
	return c, s, components[0], capture
}
func TestOriginalCaptureAndScope(t *testing.T) {
	c, s, v, b := fixture(t)
	raw, _ := json.Marshal(s)
	if _, _, e := DecodeSnapshot(raw, c, time.Now()); e != nil {
		t.Fatal(e)
	}
	r, blobs, e := Capture(c, s, b)
	if e != nil {
		t.Fatal(e)
	}
	if r.SimulationNS != 2000000000 || r.OriginalUnixMS != v.Timing.SampleTime.UnixMilli() || len(blobs) != 4 || len(r.RoomIDs) != 1 {
		t.Fatal(r)
	}
	for _, change := range []func(*Config){func(c *Config) { c.RunID = "other" }, func(c *Config) { c.Frame.Revision = "v2" }, func(c *Config) { c.Site = "other" }, func(c *Config) { c.MissionID = "other" }, func(c *Config) { c.SpecSHA256 = strings.Repeat("f", 64) }} {
		bad := c
		change(&bad)
		if _, _, e = DecodeSnapshot(raw, bad, time.Now()); e == nil {
			t.Fatal("scope admitted")
		}
	}
	var corrupt map[string]json.RawMessage
	json.Unmarshal(b, &corrupt)
	corrupt["media"] = json.RawMessage(`{}`)
	bad, _ := json.Marshal(corrupt)
	if _, _, e = Capture(c, s, bad); e == nil {
		t.Fatal("missing original admitted")
	}
	s.Patrol.Rooms[0].Evidence = nil
	if _, _, e = Capture(c, s, b); e == nil {
		t.Fatal("unattributed capture called a room visit")
	}
}
func TestHistoricalRepairDoesNotReviveLiveAndRejectsTampering(t *testing.T) {
	c, s, v, b := fixture(t)
	r, _, e := Capture(c, s, b)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	historical, e := Component(c, r, 4, 2, now)
	if e != nil {
		t.Fatal(e)
	}
	live := Live(c, s, v, 4, 1)
	p, reports, rejected := Select(c, []world.Component{live, historical}, now.Add(6*time.Second))
	if p != nil || len(reports) != 1 || rejected != 1 {
		t.Fatalf("stale live revived: %v %v %d", p, reports, rejected)
	}
	for _, change := range []func(*world.Component){func(v *world.Component) { v.Key.Source.App = "other" }, func(v *world.Component) { v.Frame.Revision = "wrong" }, func(v *world.Component) { v.Provenance.Origin = world.Simulated }, func(v *world.Component) { v.TTL += time.Second }, func(v *world.Component) {
		var r Report
		json.Unmarshal(v.Payload, &r)
		r.SimulationNS++
		v.Payload, _ = json.Marshal(r)
	}} {
		bad := historical
		change(&bad)
		_, r, n := Select(c, []world.Component{bad}, now)
		if len(r) != 0 || n != 1 {
			t.Fatal("tampered historical report admitted")
		}
	}
	_, r2, _ := Select(c, []world.Component{historical}, time.UnixMilli(c.RetainUntilMS))
	if len(r2) != 0 {
		t.Fatal("expired report revived")
	}
}

type fakeBackend struct {
	epochs  int
	uploads int
	bodies  [][]byte
	fail    bool
}

func (f *fakeBackend) NextEpoch(context.Context) (uint64, error) {
	f.epochs++
	return uint64(f.epochs), nil
}
func (f *fakeBackend) Upload(context.Context, []byte, string, time.Time) error {
	f.uploads++
	return nil
}
func (f *fakeBackend) Publish(_ context.Context, b []byte) error {
	f.bodies = append(f.bodies, bytes.Clone(b))
	if f.fail {
		return errors.New("lost acknowledgement")
	}
	return nil
}
func credentials(t *testing.T, c Config) client.Config {
	t.Helper()
	_, root, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	g, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "pub", Tenant: c.Tenant, Site: c.Site, Domain: "simulation", Principal: "publisher", Kind: "app", Source: c.Source, PublicKey: pub, Scopes: []string{"publish", "evidence-write"}, PublishAudience: "node-1", NotBeforeUnixMS: time.Now().Add(-time.Minute).UnixMilli(), NotAfterUnixMS: c.RetainUntilMS}, root)
	if e != nil {
		t.Fatal(e)
	}
	return client.Config{URL: "http://127.0.0.1:1", Audience: "node-1", Grant: g, PrivateKey: key, AllowInsecureSimulation: true}
}
func TestUncertainPublicationRestartRetriesExactOriginAndNeverDuplicates(t *testing.T) {
	ctx := context.Background()
	c, s, _, b := fixture(t)
	r, blobs, e := Capture(c, s, b)
	if e != nil {
		t.Fatal(e)
	}
	creds := credentials(t, c)
	backend := &fakeBackend{fail: true}
	path := filepath.Join(t.TempDir(), "journal.json")
	p, e := OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Report(ctx, r, blobs, time.Now()); e == nil {
		t.Fatal("expected uncertain ack")
	}
	if p.Count() != 0 || backend.uploads != 4 {
		t.Fatal("premature completion")
	}
	if _, e = OpenPublisher(ctx, c, creds, backend, path); e == nil {
		t.Fatal("concurrent owner")
	}
	body := backend.bodies[0]
	p.Close()
	backend.fail = false
	p, e = OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if e = p.Report(ctx, r, blobs, time.Now().Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if backend.epochs != 1 || backend.uploads != 4 || p.Count() != 1 || !bytes.Equal(body, backend.bodies[1]) {
		t.Fatal("restart changed origin or uploaded duplicate")
	}
	if e = p.Report(ctx, r, blobs, time.Now()); e != nil || len(backend.bodies) != 2 {
		t.Fatal("duplicate publication")
	}
	changed := r
	changed.OriginalUnixMS--
	changed.ContentSHA256 = changed.ContentDigest()
	if changed.ID != r.ID {
		t.Fatal("logical identity changed")
	}
	if e = p.Report(ctx, changed, blobs, time.Now()); e == nil {
		t.Fatal("immutable report changed")
	}
}
func TestNoFreshnessExtensionOnPublisherRestart(t *testing.T) {
	c, s, v, _ := fixture(t)
	backend := &fakeBackend{}
	creds := credentials(t, c)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.json")
	p, e := OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Progress(ctx, s, v, time.Now()); e != nil {
		t.Fatal(e)
	}
	p.Close()
	p, e = OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if e = p.Progress(ctx, s, v, time.Now().Add(20*time.Second)); e != nil {
		t.Fatal(e)
	}
	if len(backend.bodies) != 1 {
		t.Fatal("old status reissued")
	}
	var signed authority.SignedRecord
	json.Unmarshal(backend.bodies[0], &signed)
	if !signed.Component.Timing.SampleTime.Equal(v.Timing.SampleTime) || signed.Component.TTL != v.TTL {
		t.Fatal("original freshness rewritten")
	}
}
func TestJournalRejectsScopeChangeAndSymlink(t *testing.T) {
	c, _, _, _ := fixture(t)
	ctx := context.Background()
	creds := credentials(t, c)
	backend := &fakeBackend{}
	path := filepath.Join(t.TempDir(), "journal.json")
	p, e := OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	p.Close()
	c.RunID = "new-run"
	if _, e = OpenPublisher(ctx, c, creds, backend, path); e == nil {
		t.Fatal("scope changed")
	}
	link := path + "-link"
	os.Symlink(path, link)
	if _, e = ReadFile(link, 2<<20); e == nil {
		t.Fatal("followed symlink")
	}
}

func TestPersistFailureBlocksPublicationAndRestartDoesNotAllocateEpoch(t *testing.T) {
	c, s, v, _ := fixture(t)
	ctx := context.Background()
	creds := credentials(t, c)
	backend := &fakeBackend{}
	path := filepath.Join(t.TempDir(), "journal.json")
	p, e := OpenPublisher(ctx, c, creds, backend, path)
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(path)
	os.Mkdir(path, 0700)
	if p.Progress(ctx, s, v, time.Now()) == nil {
		t.Fatal("persistence failure ignored")
	}
	if p.Progress(ctx, s, v, time.Now()) == nil || len(backend.bodies) > 0 {
		t.Fatal("faulted publisher emitted bytes")
	}
	p.Close()
	if _, e = OpenPublisher(ctx, c, creds, backend, path); e == nil || backend.epochs != 1 {
		t.Fatal("corrupt journal silently replaced")
	}
}

func TestTerminalReportRequiresMatchingOutcomeAndArrival(t *testing.T) {
	c, s, v, _ := fixture(t)
	s.Patrol.State = "completed"
	s.Task.Status.Phase = mission.Failed
	if _, e := Result(c, s, v); e == nil {
		t.Fatal("mismatched result accepted")
	}
	s.Task.Status.Phase = mission.Succeeded
	if _, e := Result(c, s, v); e == nil {
		t.Fatal("completion without original arrival accepted")
	}
	s.Task.PatrolReturnResult = &tasking.PatrolReturnResult{}
	if _, e := Result(c, s, v); e != nil {
		t.Fatal(e)
	}
	s.Patrol.State = "stopped"
	s.Task.Status.Phase = mission.Canceled
	s.Task.PatrolReturnResult = nil
	if _, e := Result(c, s, v); e != nil {
		t.Fatal(e)
	}
}

func TestProgressUsesStampedStateNotNewerUnstampedStatus(t *testing.T) {
	c, s, v, _ := fixture(t)
	s.Patrol.State = "planning"
	raw, _ := json.Marshal(s)
	decoded, original, e := DecodeSnapshot(raw, c, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if decoded.Patrol.State != "waiting" || original.Version != v.Version {
		t.Fatal("unstamped live status replaced original")
	}
}

func TestReaderRejectsChangedBodyAfterRestartAndNeverRestoresConflict(t *testing.T) {
	c, s, _, b := fixture(t)
	r, _, e := Capture(c, s, b)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "history.json")
	h, e := OpenHistory(path, c, "reader")
	if e != nil {
		t.Fatal(e)
	}
	if got, e := h.Admit(c, []Report{r}); e != nil || len(got) != 1 {
		t.Fatal(e)
	}
	h.Close()
	h, e = OpenHistory(path, c, "reader")
	if e != nil {
		t.Fatal(e)
	}
	changed := r
	changed.RoomIDs = []string{"other-room"}
	changed.ContentSHA256 = changed.ContentDigest()
	if changed.ID != r.ID {
		t.Fatal("logical ID changed")
	}
	if got, e := h.Admit(c, []Report{changed}); !errors.Is(e, ErrHistoryConflict) || len(got) != 0 {
		t.Fatal("changed historical body admitted")
	}
	h.Close()
	h, e = OpenHistory(path, c, "reader")
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if got, e := h.Admit(c, []Report{r}); !errors.Is(e, ErrHistoryConflict) || len(got) != 0 {
		t.Fatal("conflicted identity restored")
	}
}

func TestNewPublisherEpochCannotRewriteReaderHistoryOrHideUnaffectedResult(t *testing.T) {
	c, s, v, b := fixture(t)
	capture, _, e := Capture(c, s, b)
	if e != nil {
		t.Fatal(e)
	}
	s.Patrol.State = "stopped"
	s.Task.Status.Phase = mission.Canceled
	result, e := Result(c, s, v)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	old, _ := Component(c, capture, 1, 1, now)
	terminal, _ := Component(c, result, 1, 2, now)
	_, original, _ := Select(c, []world.Component{old, terminal}, now)
	path := filepath.Join(t.TempDir(), "history.json")
	h, e := OpenHistory(path, c, "reader")
	if e != nil {
		t.Fatal(e)
	}
	if got, e := h.Admit(c, original); e != nil || len(got) != 2 {
		t.Fatal(e)
	}
	h.Close()
	changed := capture
	changed.RoomIDs = []string{"other-room"}
	changed.ContentSHA256 = changed.ContentDigest()
	replacement, _ := Component(c, changed, 2, 1, now)
	_, arriving, rejected := Select(c, []world.Component{replacement, terminal}, now)
	if rejected != 0 || len(arriving) != 2 {
		t.Fatal("new epoch input was not eligible for identity check")
	}
	h, e = OpenHistory(path, c, "reader")
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	got, e := h.Admit(c, arriving)
	if !errors.Is(e, ErrHistoryConflict) || len(got) != 1 || got[0].ID != result.ID {
		t.Fatal("conflict replaced history or hid unaffected result")
	}
	got, e = h.Admit(c, original)
	if !errors.Is(e, ErrHistoryConflict) || len(got) != 1 || got[0].ID != result.ID {
		t.Fatal("original replay cleared conflict")
	}
}
