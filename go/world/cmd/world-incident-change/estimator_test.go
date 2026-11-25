package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/incidentchange"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownerConfig(now time.Time) incidentchange.Config {
	source := func(id string) world.Source {
		return world.Source{Device: id, App: "test", Service: "source", Stream: "state"}
	}
	g := gasreplica.Config{Tenant: "lab", Anchor: now.Add(-2 * time.Second).UTC(), Field: gasfield.Config{Site: "site", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Simulation: world.SimulationContext{RunID: "run", ScenarioVersion: "v1", ClockDomain: "simulation:run", Seed: 31}, Min: [2]float64{-2, -2}, Max: [2]float64{2, 2}, Step: .1, Background: 2}, Assumptions: gasfield.Assumptions{Revision: "wind", WindRadians: .25, Downwind: 3, Upwind: 1, Crosswind: 1.3}, Gateways: map[string]world.Source{"east": source("east"), "west": source("west")}}
	for i, p := range [][2]float64{{-2, -1.6}, {2, -1.6}, {-2, 1.6}, {2, 1.6}} {
		id := fmt.Sprintf("gas-%d", i)
		g.Field.Sensors = append(g.Field.Sensors, gasfield.Sensor{ID: id, Source: source(id), Position: p, Calibration: "c1", SensorType: "scalar-v1"})
	}
	v := fusion.VisualConfig{Tenant: g.Tenant, Site: g.Field.Site, Frame: g.Field.Frame, Anchor: g.Anchor, Simulation: g.Field.Simulation, Package: strings.Repeat("a", 64), Calibration: "c1", Sources: map[string]world.Source{}, Models: map[string]string{}}
	for _, id := range []string{"camera-a", "camera-b", "lidar-site"} {
		v.Sources[id] = source(id)
		ss := v.Sources[id]
		ss.Service = id
		v.Sources[id] = ss
		v.Models[id] = "fixture"
	}
	tc := gaschange.Config{Public: g, MinSpan: .6, MaxGap: 1.5, Rate: 1}
	for _, sensor := range g.Field.Sensors {
		tc.Sensors = append(tc.Sensors, sensor.ID)
	}
	return incidentchange.Config{Spatial: incident.Config{Gas: g, Visual: v, VisualPublisher: source("visual"), Readers: map[string]world.Source{"east": source("reader-east"), "west": source("reader-west")}}, Temporal: tc}
}
func TestActualNodeSingleEpochAndRestartWithdrawal(t *testing.T) {
	for _, paired := range []bool{false, true} {
		name := "separate"
		if paired {
			name = "paired"
		}
		t.Run(name, func(t *testing.T) { actualNodeSingleEpochAndRestart(t, paired) })
	}
}
func actualNodeSingleEpochAndRestart(t *testing.T, paired bool) {
	ctx := context.Background()
	now := time.Now()
	cfg := ownerConfig(now)
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	pub, root, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	g := cfg.Spatial.Gas
	st, e := store.Open(filepath.Join(dir, "world"), world.Config{Tenant: g.Tenant, Site: g.Field.Site, ClockDomain: g.Field.Simulation.ClockDomain, Now: time.Now, MaxEntries: 64, MaxFutureSkew: time.Second, AllowSynthetic: true, Frames: []world.FrameRef{g.Field.Frame}})
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	verifier, e := authority.NewVerifier(authority.Config{RootPublicKey: pub, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Audience: "node", Now: time.Now, Replay: authority.NewMemoryReplay(4096)})
	if e != nil {
		t.Fatal(e)
	}
	rev, e := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()}, root)
	if e != nil {
		t.Fatal(e)
	}
	if e = verifier.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
	n, e := node.New(node.Config{Tenant: g.Tenant, Site: g.Field.Site, Audience: "node", ProofPath: filepath.Join(dir, "proofs"), MaxRecords: 64}, st, verifier)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(n.Handler())
	defer server.Close()
	credential := func(source world.Source) client.Config {
		p, k, _ := ed25519.GenerateKey(rand.Reader)
		grant, e := authority.SignGrant(authority.Grant{Version: authority.Version, ID: source.Device, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Principal: source.Device, Kind: "app", Source: source, PublicKey: p, Scopes: []string{"read", "publish"}, PublishAudience: "node", NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}, root)
		if e != nil {
			t.Fatal(e)
		}
		return client.Config{URL: server.URL, Audience: "node", Grant: grant, PrivateKey: k, AllowInsecureSimulation: true}
	}
	rc := runtimeConfig{Public: cfg, Gateway: "east", Client: credential(g.Gateways["east"])}
	trustPath := ""
	if paired {
		trustPath = filepath.Join(dir, "member-trust.json")
		raw, _ := json.Marshal(pairTrust{pub, rev})
		if e = os.WriteFile(trustPath, raw, 0600); e != nil {
			t.Fatal(e)
		}
		rc.TrustPath = trustPath
	}
	operation := "tick"
	if paired {
		operation = "paired"
	}

	r, e := newRuntime(rc)
	if e != nil {
		t.Fatal(e)
	}
	publishers := []*publisher{}
	for _, s := range g.Field.Sensors {
		p, e := newPublisher(credential(s.Source))
		if e != nil {
			t.Fatal(e)
		}
		publishers = append(publishers, p)
	}
	for index := 0; index < 3; index++ {
		at := g.Seconds(time.Now()) - float64(2-index)*.4
		for i, s := range g.Field.Sensors {
			version, e := publishers[i].version(ctx)
			if e != nil {
				t.Fatal(e)
			}
			sim := g.Field.Simulation
			o := gasfield.Observation{ID: fmt.Sprintf("%s/%d", s.ID, version.Sequence), Sensor: s.ID, Source: s.Source, Version: version, Site: g.Field.Site, Frame: g.Field.Frame, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &sim}, Calibration: s.Calibration, SensorType: s.SensorType, Units: "ppm", Species: "synthetic-tracer", Captured: at, ValidUntil: at + 4, Value: 2 + 120*gasfield.Kernel(s.Position, [2]float64{-.8, -.4}, g.Assumptions), Uncertainty: 1}
			c, e := gasreplica.ObservationComponent(g, o)
			if e != nil {
				t.Fatal(e)
			}
			p, e := publishers[i].prepare(c)
			if e != nil {
				t.Fatal(e)
			}
			if e = publishers[i].send(ctx, p); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = r.step(ctx, "sample"); e != nil {
			t.Fatal(e)
		}
	}
	first, e := r.step(ctx, operation)
	if e != nil {
		t.Fatal(e)
	}
	_ = first
	snapshot, e := r.p.api.Query(ctx)
	if e != nil {
		t.Fatal(e)
	}
	own := map[string]world.Component{}
	for _, c := range snapshot.Components {
		if c.Key.Source == g.Gateways["east"] {
			own[c.Key.Type] = c
		}
	}
	if !paired && (len(own) != 2 || own[gasreplica.ClaimType].Version.Epoch == 0 || own[gasreplica.ClaimType].Version.Epoch != own[gaschange.ClaimType].Version.Epoch) {
		t.Fatalf("one owner did not publish both types: %+v", own)
	}
	if paired {
		if r.p.last == nil || r.p.last.component.Key.Type != incidentchange.PairType || len(r.p.last.body) > 64<<10 {
			t.Fatal("actual outer paired envelope exceeds transport bound")
		}
		t.Logf("actual admitted outer pair: %d bytes, epoch %d", len(r.p.last.body), r.p.last.component.Version.Epoch)
		if len(own) != 1 {
			t.Fatalf("atomic path published partial records: %+v", own)
		}
		if _, e = incidentchange.ValidatePair(cfg, own[incidentchange.PairType], time.Now(), r.verifyMembers); e != nil {
			t.Fatal(e)
		}
	}
	reader, e := newRuntime(runtimeConfig{Public: cfg, Reader: "east", Client: credential(cfg.Spatial.Readers["east"]), TrustPath: trustPath})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reader.step(ctx, "publish"); e == nil {
		t.Fatal("publication without an observed view succeeded")
	}
	observed, e := reader.step(ctx, "observe")
	if e != nil {
		t.Fatal(e)
	}
	if observed.(map[string]any)["published"] != false || reader.p.epoch != 0 || reader.observedResult == nil {
		t.Fatal("read-only observation allocated publication authority")
	}
	before := own[gaschange.ClaimType]
	if paired {
		before = own[incidentchange.PairType]
	}
	if gasreplica.Remaining(before, time.Now()) <= 0 {
		t.Fatal("test missed live predecessor window")
	}
	restarted, e := newRuntime(rc)
	if e != nil {
		t.Fatal(e)
	}
	reply, e := restarted.step(ctx, operation)
	if e != nil {
		t.Fatal(e)
	}
	result := reply.(map[string]any)
	if state := result["temporal"].(gaschange.Result).Window.State; state != "warming" && state != "unknown" {
		t.Fatal("restart reconstructed temporal history", state)
	}
	// Store repair records include the authoritative tombstone even when reader queries
	// omit an unavailable current value. It cannot extend the original support.
	all, e := st.Records()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, c := range all {
		if c.Key == before.Key {
			found = true
			if !c.Tombstone || c.Version.Epoch <= before.Version.Epoch || !c.Timing.SampleTime.Add(c.TTL).Equal(before.Timing.SampleTime.Add(before.TTL)) {
				t.Fatalf("restart did not withdraw exact predecessor lifetime: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("restart tombstone missing")
	}

	if paired {
		different, _, _ := ed25519.GenerateKey(rand.Reader)
		raw, _ := json.Marshal(pairTrust{different, rev})
		os.WriteFile(trustPath, raw, 0600)
		if e = reader.refreshTrust(); e == nil {
			t.Fatal("reader replaced pinned trust root")
		}
		revoked := rev.Revocations
		revoked.Revision++
		revoked.GrantIDs = []string{rc.Client.Grant.Grant.ID}
		signed, e := authority.SignRevocations(revoked, root)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ = json.Marshal(pairTrust{pub, signed})
		os.WriteFile(trustPath, raw, 0600)
		if e = reader.refreshTrust(); e != nil {
			t.Fatal(e)
		}
		if _, e = reader.memberVerifier.VerifyGrant(rc.Client.Grant, "publish"); e == nil {
			t.Fatal("current origin grant revocation ignored")
		}
		if _, e = incidentchange.ValidatePair(cfg, before, time.Now(), reader.verifyMembers); e == nil {
			t.Fatal("current member revocation ignored")
		}
		raw, _ = json.Marshal(pairTrust{pub, rev})
		os.WriteFile(trustPath, raw, 0600)
		if e = reader.refreshTrust(); e == nil {
			t.Fatal("revocation rollback accepted")
		}
		reader.cfg.Watch = true
		reader.startWatch(ctx)
		defer reader.stopWatch()
		watchCtx, cancelWatch := context.WithTimeout(ctx, 2*time.Second)
		defer cancelWatch()
		if _, e = reader.snapshot(watchCtx); e != nil {
			t.Fatal("actual authenticated Watch failed", e)
		}
		denied := rev.Revocations
		denied.Revision = 3
		denied.GrantIDs = []string{rc.Client.Grant.Grant.ID, reader.cfg.Client.Grant.Grant.ID}
		current, e := authority.SignRevocations(denied, root)
		if e != nil {
			t.Fatal(e)
		}
		if e = verifier.UpdateRevocations(current); e != nil {
			t.Fatal(e)
		}
		end := time.Now().Add(time.Second)
		closed := false
		for time.Now().Before(end) {
			reader.watch.mu.Lock()
			closed = !reader.watch.ready && reader.watch.failure != nil
			reader.watch.mu.Unlock()
			if closed {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !closed {
			t.Fatal("actual revoked read grant retained Watch snapshot")
		}
		if _, e = reader.snapshot(watchCtx); e == nil {
			t.Fatal("revoked Watch delivered cached current data")
		}
	}
}

func TestEstimatorExpiredPendingReturnsUnknownWithoutSubmitting(t *testing.T) {
	cfg := ownerConfig(time.Now())
	estimator, err := gaschange.New(cfg.Temporal)
	if err != nil {
		t.Fatal(err)
	}
	old := world.Component{Timing: world.Timing{SampleTime: time.Now().Add(-2 * time.Second)}, TTL: time.Second}
	r := &runtime{cfg: runtimeConfig{Public: cfg, Gateway: "east"}, p: &publisher{pending: &signedPending{component: old}}, estimator: estimator, streams: map[string]*claimStream{incidentchange.PairType: {digest: "previous", possible: &old}}}
	// No API client exists: any attempted retry/query/publication would panic.
	reply, err := r.estimate(context.Background(), "paired")
	if err != nil {
		t.Fatal(err)
	}
	out := reply.(map[string]any)
	change := out["temporal"].(gaschange.Result)
	if change.Window.State != "unknown" || len(change.Inputs) != 0 || len(change.Window.Traces) != 0 || change.Window.ValidUntil != change.Window.Evaluated || r.estimator == estimator || r.p.pending != nil || out["expired_payload_submitted"] != false || out["publication_complete"] != false || len(out["publications"].([]map[string]any)) != 0 {
		t.Fatalf("expired work retained as current: %+v", out)
	}
	if r.streams[incidentchange.PairType].digest != "" || r.streams[incidentchange.PairType].possible != &old {
		t.Fatal("old withdrawal ownership lost or stale digest retained")
	}
	// An error with identical text is not the typed local-expiry boundary.
	denied := errors.New("original support expired")
	if reply, err = r.expiredEstimate(denied, nil); reply != nil || !errors.Is(err, denied) {
		t.Fatal("untyped authority/schema error downgraded")
	}
	accepted := []map[string]any{{"type": gasreplica.ClaimType, "sha256": "already-acknowledged"}}
	reply, err = r.expiredEstimate(fmt.Errorf("before submission: %w", errOriginalExpired), accepted)
	if err != nil || len(reply.(map[string]any)["publications"].([]map[string]any)) != 1 {
		t.Fatal("earlier accepted separate claim hidden")
	}
}
