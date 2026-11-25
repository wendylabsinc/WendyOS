package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
)

func archiveConfig(c *Config) {
	accessConfig(c)
	c.ProofArchivePath = filepath.Join(filepath.Dir(c.JournalPath), "archive", "proofs.db")
	c.CreateProofArchive = true
	raw := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	result := serviceaccess.Source{App: "com.example.fusion", Service: "worker", ComponentType: resultrecord.ComponentType, Stream: "results"}
	c.Policy.Grants = append(c.Policy.Grants, Grant{App: result.App, Service: result.Service, Publish: []serviceorigin.PublishScope{{ComponentType: result.ComponentType, Stream: result.Stream}}, Subscribe: []serviceaccess.Source{raw}})
	c.Policy.Grants[2].Subscribe = append(c.Policy.Grants[2].Subscribe, result)
}
func archiveReference(t *testing.T, reply *appv1.PublishReply) proofref.Reference {
	t.Helper()
	signed, r := decodeReply(t, reply)
	return exactReference(reply.SignedRecordJson, serviceorigin.VerifiedRecord{Record: r, Bytes: signed.Record})
}
func proofRequest(ref proofref.Reference) *appv1.GetRecordProofRequest {
	return &appv1.GetRecordProofRequest{Source: accessSelector(sourceSelector(ref)), Entity: ref.Key.Entity, Epoch: ref.Version.Epoch, Sequence: ref.Version.Sequence, EnvelopeSha256: ref.EnvelopeSHA256, RecordSha256: ref.RecordSHA256}
}
func readProof(t *testing.T, client appv1.WorldAppServiceClient, ref proofref.Reference, want []byte, until int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := client.GetRecordProof(ctx, proofRequest(ref))
	if err != nil || got == nil || !got.Historical || !bytes.Equal(got.SignedRecordJson, want) || got.RetainUntilUnixMs < until {
		t.Fatal("exact historical proof unavailable or rewritten", err)
	}
}
func derivedRequest(t *testing.T, refs []proofref.Reference, ttl time.Duration, until int64) *appv1.PublishRequest {
	t.Helper()
	payload, err := resultrecord.Encode(resultrecord.Result{Version: 1, ResultSchema: "example.gas-alert.v1", ResultJSON: []byte("{ \"label\" : \"gas elevated\", \"confidence\" : 0.900 }\n"), Inputs: refs, RetainUntilUnixMS: until})
	if err != nil {
		t.Fatal(err)
	}
	provenance := world.Provenance{Origin: world.Inferred, AppVersion: "fusion-1", ModelID: "gas-rule", ModelVersion: "rule-1"}
	for _, ref := range refs {
		provenance.Inputs = append(provenance.Inputs, world.Input{ID: ref.ID(), Origin: world.Measured})
	}
	p, err := appv1.EncodePublish(serviceorigin.PublishRequest{Entity: "gas-alert", ComponentType: resultrecord.ComponentType, Stream: "results", Frame: world.FrameRef{ID: "floor", Revision: "survey-1"}, Timing: world.Timing{SampleTime: moduleTestNow, ClockDomain: "utc", ErrorKnown: true}, TTL: ttl, Provenance: provenance, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func archiveWorker(t *testing.T, f *moduleFixture, subscribe bool) appv1.WorldAppServiceClient {
	t.Helper()
	g := f.config.Policy.Grants[3]
	var subscriptions []serviceaccess.Source
	if subscribe {
		subscriptions = g.Subscribe
	}
	mount, err := f.manager.EnsureAccess(context.Background(), g.App, g.Service, 8, g.Publish, subscriptions)
	if err != nil {
		t.Fatal(err)
	}
	return moduleDial(t, mount.SocketPath)
}

func TestArchiveReplacementBeforeResultAndHistoricalRestart(t *testing.T) {
	f := setupModule(t, archiveConfig)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	first := modulePublish(t, writer, moduleRequest("gas"))
	oldRef := archiveReference(t, first)
	next := moduleRequest("gas")
	next.PayloadJson = bytes.ReplaceAll(next.PayloadJson, []byte("12.5"), []byte("14.5"))
	newer := modulePublish(t, writer, next)
	if archiveReference(t, newer).Version.Sequence <= oldRef.Version.Sequence {
		t.Fatal("source did not advance")
	}
	worker := archiveWorker(t, f, true)
	until := moduleTestNow.Add(2 * time.Minute).UnixMilli()
	claim := modulePublish(t, worker, derivedRequest(t, []proofref.Reference{oldRef}, 10*time.Second, until))
	claimRef := archiveReference(t, claim)
	rawSource := sourceSelector(oldRef)
	claimSource := sourceSelector(claimRef)
	reader := moduleDial(t, accessMount(t, f, rawSource, claimSource).SocketPath)
	page := accessQuery(t, reader, []serviceaccess.Source{rawSource, claimSource}, 8, "")
	if len(page.SignedRecordJson) != 2 {
		t.Fatal("live result/source absent", len(page.SignedRecordJson))
	}
	for _, raw := range page.SignedRecordJson {
		if bytes.Equal(raw, first.SignedRecordJson) {
			t.Fatal("replaced raw value returned as current")
		}
	}
	readProof(t, reader, oldRef, first.SignedRecordJson, until)
	readProof(t, reader, claimRef, claim.SignedRecordJson, until)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, e := writer.GetRecordProof(ctx, proofRequest(oldRef)); e == nil || out != nil {
		t.Fatal("writer inherited read permission")
	}
	wrong := proofRequest(oldRef)
	wrong.Sequence++
	if out, e := reader.GetRecordProof(ctx, wrong); e == nil || out != nil {
		t.Fatal("partial reference matched a different version")
	}
	f.clock.mu.Lock()
	f.clock.window.Earliest = f.clock.window.Earliest.Add(40 * time.Second)
	f.clock.window.Latest = f.clock.window.Latest.Add(40 * time.Second)
	f.clock.window.Monotonic += 40 * time.Second
	f.clock.mu.Unlock()
	f.manager.deps.SampleNow = func() time.Time { return moduleTestNow.Add(40 * time.Second) }
	if got := accessQuery(t, reader, []serviceaccess.Source{rawSource, claimSource}, 8, ""); len(got.SignedRecordJson) != 0 {
		t.Fatal("retention renewed expired live data")
	}
	readProof(t, reader, oldRef, first.SignedRecordJson, until)
	if e := f.manager.Close(); e != nil {
		t.Fatal(e)
	}
	f.config.CreateJournal = false
	f.config.CreateProofArchive = false
	f.deps.SampleNow = func() time.Time { return moduleTestNow.Add(40 * time.Second) }
	var err error
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	reader = moduleDial(t, accessMount(t, f, rawSource, claimSource).SocketPath)
	// Only a fresh reader is active: old source authority is not restored.
	readProof(t, reader, oldRef, first.SignedRecordJson, until)
	readProof(t, reader, claimRef, claim.SignedRecordJson, until)
	if got := accessQuery(t, reader, []serviceaccess.Source{rawSource, claimSource}, 8, ""); len(got.SignedRecordJson) != 0 {
		t.Fatal("reopen restored historical publication authority")
	}
}

func TestArchiveResultRequiresCurrentReadGrantAndExactInputs(t *testing.T) {
	f := setupModule(t, archiveConfig)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	source := modulePublish(t, writer, moduleRequest("gas"))
	ref := archiveReference(t, source)
	worker := archiveWorker(t, f, false)
	until := moduleTestNow.Add(time.Minute).UnixMilli()
	request := derivedRequest(t, []proofref.Reference{ref}, time.Second, until)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := worker.Publish(ctx, request); err == nil || out != nil {
		t.Fatal("unrequested subscription authorized a claim")
	}
	worker = archiveWorker(t, f, true)
	for _, change := range []func(*proofref.Reference){func(r *proofref.Reference) { r.Version.Sequence++ }, func(r *proofref.Reference) { r.RecordSHA256 = string(bytes.Repeat([]byte("a"), 64)) }, func(r *proofref.Reference) { r.Key.Source.Stream = "other" }} {
		bad := ref
		change(&bad)
		req := derivedRequest(t, []proofref.Reference{bad}, time.Second, until)
		if out, err := worker.Publish(ctx, req); err == nil || out != nil {
			t.Fatal("claim accepted an altered exact reference")
		}
	}
	if out, err := worker.Publish(ctx, derivedRequest(t, []proofref.Reference{ref}, 31*time.Second, until)); err == nil || out != nil {
		t.Fatal("derived lifetime exceeded raw source")
	}
	accepted := modulePublish(t, worker, request)
	f.mount(t, 0, 2)
	reader := moduleDial(t, accessMount(t, f, sourceSelector(ref), sourceSelector(archiveReference(t, accepted))).SocketPath)
	got := accessQuery(t, reader, []serviceaccess.Source{sourceSelector(archiveReference(t, accepted))}, 8, "")
	if len(got.SignedRecordJson) != 0 {
		t.Fatal("result survived original source authority replacement")
	}
	readProof(t, reader, ref, source.SignedRecordJson, until)
}

func TestArchiveHistoricalReplyRechecksRetention(t *testing.T) {
	f := setupModule(t, archiveConfig)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	source := modulePublish(t, writer, moduleRequest("gas"))
	ref := archiveReference(t, source)
	accessMount(t, f, sourceSelector(ref))
	scope := f.config.Policy.scope(Grant{App: "com.example.fusion", Service: "reader"}, 7)
	response := &appv1.GetRecordProofReply{SignedRecordJson: source.SignedRecordJson, Historical: true, RetainUntilUnixMs: moduleTestNow.UnixMilli()}
	if got, err := f.manager.finalizeResponse(context.Background(), scope, response); err == nil || got != nil {
		t.Fatal("finalizer released expired retained bytes")
	}
	copy := proto.Clone(response).(*appv1.GetRecordProofReply)
	copy.RetainUntilUnixMs = moduleTestNow.Add(time.Minute).UnixMilli()
	copy.Historical = false
	if got, err := f.manager.finalizeResponse(context.Background(), scope, copy); err == nil || got != nil {
		t.Fatal("historical reply presented as live")
	}
}

func TestArchiveDisabledRefusesDerivedPublication(t *testing.T) {
	f := setupModule(t, func(c *Config) { archiveConfig(c); c.ProofArchivePath = ""; c.CreateProofArchive = false })
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	source := modulePublish(t, writer, moduleRequest("gas"))
	ref := archiveReference(t, source)
	worker := archiveWorker(t, f, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if got, err := worker.Publish(ctx, derivedRequest(t, []proofref.Reference{ref}, time.Second, moduleTestNow.Add(time.Minute).UnixMilli())); err == nil || got != nil {
		t.Fatal("derived publication bypassed required durable archive")
	}
}

func TestArchiveMediaRetentionAndShortestInputBound(t *testing.T) {
	imageSource := accessSource("telemetry-a", "wendy.sensor.image.v1", "front")
	f := setupModule(t, func(c *Config) {
		archiveConfig(c)
		c.Policy.Grants[3].Subscribe = append(c.Policy.Grants[3].Subscribe, imageSource)
	})
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	gas := modulePublish(t, writer, moduleRequest("gas"))
	gasRef := archiveReference(t, gas)
	// The legacy media contract binds size/type/digest; calibrated capture media
	// validation has its own tests. This test exercises retention independence.
	data := []byte("legacy image reference fixture")
	sha := proofDigest(data)
	mediaUntil := moduleTestNow.Add(45 * time.Second).UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upload, e := writer.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: imageSource.ComponentType, Stream: imageSource.Stream, Sha256: sha, Size: int64(len(data)), Data: data, ContentType: "image/jpeg", RetainUntilUnixMs: mediaUntil})
	if e != nil || upload == nil || !upload.Complete {
		t.Fatal("source evidence upload failed", e)
	}
	request := moduleRequest("image")
	request.TtlNs = uint64(2 * time.Second)
	request.PayloadJson, _ = json.Marshal(mediaPayload{SHA256: sha, ContentType: "image/jpeg", Size: int64(len(data)), CalibrationRevision: "camera-cal-1"})
	imageReply := modulePublish(t, writer, request)
	imageRef := archiveReference(t, imageReply)
	worker := archiveWorker(t, f, true)
	refs := []proofref.Reference{gasRef, imageRef}
	if out, e := worker.Publish(ctx, derivedRequest(t, refs, time.Second, moduleTestNow.Add(time.Minute).UnixMilli())); e == nil || out != nil {
		t.Fatal("proof pin silently extended original media retention")
	}
	until := moduleTestNow.Add(40 * time.Second).UnixMilli()
	if out, e := worker.Publish(ctx, derivedRequest(t, refs, 3*time.Second, until)); e == nil || out != nil {
		t.Fatal("result ignored its shortest input sample bound")
	}
	claim := modulePublish(t, worker, derivedRequest(t, refs, time.Second, until))
	claimRef := archiveReference(t, claim)
	reader := moduleDial(t, accessMount(t, f, imageSource, sourceSelector(claimRef)).SocketPath)
	readProof(t, reader, imageRef, imageReply.SignedRecordJson, until)
	media, e := reader.GetEvidence(ctx, &appv1.GetEvidenceRequest{Source: accessSelector(imageSource), Sha256: sha, Length: int32(len(data))})
	if e != nil || media.RetainUntilUnixMs != mediaUntil || !bytes.Equal(media.Data, data) {
		t.Fatal("claim pin changed media bytes/deadline", e)
	}
	// A final query checks the source clock again, independent of retained proof
	// and media lifetimes. Gas remains fresh; the image and result have expired.
	f.manager.deps.SampleNow = func() time.Time { return moduleTestNow.Add(3 * time.Second) }
	page := accessQuery(t, reader, []serviceaccess.Source{sourceSelector(claimRef)}, 8, "")
	if len(page.SignedRecordJson) != 0 {
		t.Fatal("query retained a result past its oldest required input")
	}
	readProof(t, reader, claimRef, claim.SignedRecordJson, until)
}

func TestArchiveAgeHintCannotRenewDerivedLifetime(t *testing.T) {
	f := setupModule(t, archiveConfig)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	request := moduleRequest("gas")
	request.Timing.AgeHintNs = uint64(20 * time.Second)
	source := modulePublish(t, writer, request)
	ref := archiveReference(t, source)
	worker := archiveWorker(t, f, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	until := moduleTestNow.Add(time.Minute).UnixMilli()
	if out, e := worker.Publish(ctx, derivedRequest(t, []proofref.Reference{ref}, 15*time.Second, until)); e == nil || out != nil {
		t.Fatal("lookup renewed original age-hint deadline")
	}
	modulePublish(t, worker, derivedRequest(t, []proofref.Reference{ref}, 5*time.Second, until))
}
