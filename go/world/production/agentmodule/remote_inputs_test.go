package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Actual signed records, native replica services and persistent stores. This
// host test does not claim Linux socket or network deployment verification.
type remoteFusionFixture struct {
	a, b           *moduleReplicaFixture
	pa, pb, fusion appservice.Publisher
	anchor         int64
}

func newRemoteFusionFixture(t *testing.T) *remoteFusionFixture {
	f := &remoteFusionFixture{anchor: moduleTestNow.Add(-150 * time.Millisecond).UnixNano()}
	f.a, f.b = newReplicaPairConfigured(t, func(_ int, c *Config) {
		c.Policy.ExecutionDomain, c.Policy.SampleClockDomain = "simulation", "simulation:run-a"
		c.CaptureSource = &CaptureSource{RunID: "run-a", BootID: "boot-original", PackageSHA256: string(bytes.Repeat([]byte("a"), 64)), AnchorUnixNS: f.anchor, Frame: c.Policy.Frames[0]}
		c.ProofArchivePath = filepath.Join(filepath.Dir(c.JournalPath), "archive", "proof.db")
		c.CreateProofArchive = true
		local := serviceaccess.Source{App: "com.example.split", Service: "gas", ComponentType: sensorrecord.ComponentType, Stream: "gas-1"}
		remote := local
		remote.Device = c.Replication.Peers[0].Device
		result := serviceaccess.Source{App: local.App, Service: "fusion", ComponentType: resultrecord.ComponentType, Stream: "result"}
		c.Policy.Grants = []Grant{
			{App: local.App, Service: local.Service, Publish: []serviceorigin.PublishScope{{ComponentType: local.ComponentType, Stream: local.Stream}}},
			{App: local.App, Service: result.Service, Publish: []serviceorigin.PublishScope{{ComponentType: result.ComponentType, Stream: result.Stream}}, Subscribe: []serviceaccess.Source{local, remote}},
			{App: local.App, Service: "reader", Subscribe: []serviceaccess.Source{local, remote, result}},
		}
		for _, list := range [][]proofref.Source{c.Replication.Peers[0].Import, c.Replication.Peers[0].Export} {
			list[0].App, list[0].Service, list[0].Component, list[0].Stream = local.App, local.Service, local.ComponentType, local.Stream
		}
	})
	startReplicaPair(t, f.a, f.b)
	f.pa, f.pb = f.a.publisher(t, 0, 1), f.b.publisher(t, 0, 1)
	f.fusion = f.a.publisher(t, 1, 1)
	return f
}

func (f *remoteFusionFixture) observations(t *testing.T, step time.Duration) []serviceorigin.SignedRecord {
	t.Helper()
	var proofs []serviceorigin.SignedRecord
	for i, node := range []*moduleReplicaFixture{f.a, f.b} {
		node.sample.Store(moduleTestNow.Add(100*time.Millisecond + step).UnixNano())
		capture, _ := fusionAppCapture(t, "gas-1", f.anchor)
		sample, err := capture.Sample()
		if err != nil {
			t.Fatal(err)
		}
		sample.SampleNS += int64(step)
		sample.Sequence += uint64(step / time.Second)
		sample.ID = fmt.Sprintf("%s/%s/%d", sample.RunID, sample.SensorID, sample.Sequence)
		captureSchemaSetSample(t, &capture, sample)
		request, err := sensorrecord.Publication(capture, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		publisher := f.pa
		if i == 1 {
			publisher = f.pb
		}
		proofs = append(proofs, node.publish(t, publisher, request))
	}
	f.a.sync(t)
	return proofs
}

func (f *remoteFusionFixture) claim(t *testing.T, proofs []serviceorigin.SignedRecord) (serviceorigin.SignedRecord, []proofref.Reference) {
	t.Helper()
	var refs []proofref.Reference
	var leaves []world.Input
	var sample time.Time
	for _, proof := range proofs {
		var r serviceorigin.Record
		if json.Unmarshal(proof.Record, &r) != nil {
			t.Fatal("record")
		}
		raw, _ := json.Marshal(proof)
		v := serviceorigin.VerifiedRecord{Record: r, Bytes: proof.Record}
		ref := exactReference(raw, v)
		refs = append(refs, ref)
		leaves = append(leaves, world.Input{ID: ref.ID(), Origin: world.Simulated, Simulation: r.Publication.Provenance.Simulation})
		sample = r.Publication.Timing.SampleTime
	}
	data, err := resultrecord.Encode(resultrecord.Result{Version: 1, ResultSchema: "example.split.v1", ResultJSON: []byte(`{"gas_elevated":true}`), Inputs: refs, RetainUntilUnixMS: moduleTestNow.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	q := serviceorigin.PublishRequest{Entity: "split-result", ComponentType: resultrecord.ComponentType, Stream: "result", Frame: f.a.config.Policy.Frames[0], Timing: world.Timing{SampleTime: sample, ClockDomain: "simulation:run-a", ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "test", ModelID: "split-rule", ModelVersion: "1", Inputs: leaves}, Payload: data}
	signed, err := f.fusion.Publish(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return signed, refs
}

func TestMixedDeviceResultRetainsExactRemoteInputAndExpires(t *testing.T) {
	f := newRemoteFusionFixture(t)
	proofs := f.observations(t, 0)
	claim, refs := f.claim(t, proofs)
	if err := f.a.m.admitArchived(context.Background(), claim); err != nil {
		t.Fatal("mixed claim", err)
	}
	raw, _ := json.Marshal(proofs[1])
	stored, err := f.a.m.archive.Get(context.Background(), refs[1].Source(), refs[1])
	if err != nil || !bytes.Equal(raw, stored.Envelope) || !stored.Historical {
		t.Fatal("foreign original changed", err)
	}
	current, err := f.a.m.Snapshot(context.Background())
	if err != nil || len(current) != 2 {
		t.Fatalf("source and mixed result: %d %v", len(current), err)
	}
	reader := f.a.publisher(t, 2, 1)
	_ = reader
	scope := f.a.config.Policy.scope(f.a.config.Policy.Grants[2], 1)
	live, err := f.a.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{claim, proofs[1]}, scope)
	if err != nil || len(live) != 2 {
		t.Fatalf("final mixed query: %d %v", len(live), err)
	}
	f.a.sample.Store(moduleTestNow.Add(4 * time.Second).UnixNano())
	current, err = f.a.m.Snapshot(context.Background())
	if err != nil || len(current) != 0 {
		t.Fatalf("expired source/result visible: %d %v", len(current), err)
	}
	if _, err := f.a.m.archive.Get(context.Background(), refs[1].Source(), refs[1]); err != nil {
		t.Fatal("expiry deleted history", err)
	}
	if err := f.a.m.admitArchived(context.Background(), claim); err == nil {
		t.Fatal("expired remote original reused")
	}
	newProofs := f.observations(t, 4*time.Second)
	fresh, _ := f.claim(t, newProofs)
	if err := f.a.m.admitArchived(context.Background(), fresh); err != nil {
		t.Fatal("fresh restore", err)
	}
	current, err = f.a.m.Snapshot(context.Background())
	if err != nil || len(current) != 2 {
		t.Fatalf("restored source/result: %d %v", len(current), err)
	}
}

func TestMixedDeviceResultRequiresReplicatedOriginalAndLiveSourceLease(t *testing.T) {
	f := newRemoteFusionFixture(t)
	proofs := f.observations(t, 0)
	claim, _ := f.claim(t, proofs)
	// A valid signature outside the current remote register is insufficient.
	var r serviceorigin.Record
	_ = json.Unmarshal(proofs[1].Record, &r)
	unreplicated, err := f.pb.Publish(context.Background(), r.Publication)
	if err != nil {
		t.Fatal(err)
	}
	unknown, _ := f.claim(t, []serviceorigin.SignedRecord{proofs[0], unreplicated})
	if err := f.a.m.admitArchived(context.Background(), unknown); err == nil {
		t.Fatal("unreplicated original accepted")
	}
	if err := f.a.m.admitArchived(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	f.b.m.Release(f.b.config.Policy.Grants[0].App, "gas", 1)
	f.a.sync(t)
	current, err := f.a.m.Snapshot(context.Background())
	if err != nil || len(current) != 1 {
		t.Fatalf("retired remote source kept result live: %d %v", len(current), err)
	}
}

func TestLinuxMixedDeviceResultUsesOrdinarySocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux peer credentials and pidfds")
	}
	f := newRemoteFusionFixture(t)
	proofs := f.observations(t, 0)
	claim, refs := f.claim(t, proofs)
	var r serviceorigin.Record
	if json.Unmarshal(claim.Record, &r) != nil {
		t.Fatal("claim record")
	}
	f.a.m.Release(f.a.config.Policy.Grants[1].App, "fusion", 1)
	g := f.a.config.Policy.Grants[1]
	mount, err := f.a.m.EnsureAccess(context.Background(), g.App, g.Service, 2, g.Publish, g.Subscribe)
	if err != nil {
		t.Fatal(err)
	}
	request, err := appv1.EncodePublish(r.Publication)
	if err != nil {
		t.Fatal(err)
	}
	accepted := modulePublish(t, moduleDial(t, mount.SocketPath), request)
	g = f.a.config.Policy.Grants[2]
	readMount, err := f.a.m.EnsureAccess(context.Background(), g.App, g.Service, 1, g.Publish, g.Subscribe)
	if err != nil {
		t.Fatal(err)
	}
	reader := moduleDial(t, readMount.SocketPath)
	source := sourceSelector(archiveReference(t, accepted))
	page := accessQuery(t, reader, []serviceaccess.Source{source, sourceSelector(refs[1])}, 8, "")
	if len(page.SignedRecordJson) != 2 {
		t.Fatal("ordinary mixed result/source query missing", len(page.SignedRecordJson))
	}
	raw, _ := json.Marshal(proofs[1])
	readProof(t, reader, refs[1], raw, moduleTestNow.Add(time.Minute).UnixMilli())
}
