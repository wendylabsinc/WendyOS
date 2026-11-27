package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
)

type remoteMediaFixture struct {
	a, b   *moduleReplicaFixture
	anchor int64
}

func newRemoteMediaFixture(t *testing.T, hook func(*moduleReplicaFixture, *ReplicaTransport)) *remoteMediaFixture {
	t.Helper()
	f := &remoteMediaFixture{anchor: moduleTestNow.Add(-150 * time.Millisecond).UnixNano()}
	f.a, f.b = newReplicaPairConfigured(t, func(_ int, c *Config) {
		c.Policy.ExecutionDomain, c.Policy.SampleClockDomain = "simulation", "simulation:run-a"
		c.CaptureSource = &CaptureSource{RunID: "run-a", BootID: "boot-original", PackageSHA256: strings.Repeat("a", 64), AnchorUnixNS: f.anchor, Frame: c.Policy.Frames[0]}
		base := filepath.Dir(c.JournalPath)
		c.EvidenceDirectory = filepath.Join(base, "evidence")
		c.ProofArchivePath, c.CreateProofArchive = filepath.Join(base, "archive", "proof.db"), true
		peer := &c.Replication.Peers[0]
		peer.EvidencePath = filepath.Join(base, "remote-evidence")
		peer.Import, peer.Export = nil, nil
		c.Policy.Grants = nil
		var subs []serviceaccess.Source
		for _, id := range []string{"camera-b", "audio-1"} {
			s := serviceaccess.Source{App: "com.example.media", Service: id, ComponentType: sensorrecord.ComponentType, Stream: id}
			c.Policy.Grants = append(c.Policy.Grants, Grant{App: s.App, Service: id, Publish: []serviceorigin.PublishScope{{ComponentType: s.ComponentType, Stream: id}}})
			local := proofref.Source{Tenant: c.Policy.Tenant, Site: c.Policy.Site, Device: c.Policy.Device, App: s.App, Service: id, Component: s.ComponentType, Stream: id}
			remote := local
			remote.Device = peer.Device
			peer.Export = append(peer.Export, local)
			peer.Import = append(peer.Import, remote)
			subs = append(subs, s)
			s.Device = peer.Device
			subs = append(subs, s)
		}
		c.Policy.Grants = append(c.Policy.Grants,
			Grant{App: "com.example.media", Service: "fusion", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "result"}}, Subscribe: subs},
			Grant{App: "com.example.media", Service: "reader", Subscribe: subs})
	})
	for _, node := range []*moduleReplicaFixture{f.a, f.b} {
		transport := node.transport()
		if hook != nil {
			hook(node, &transport)
		}
		if err := node.m.StartReplication(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func mediaSource(f *moduleReplicaFixture, id string) serviceaccess.Source {
	return serviceaccess.Source{Device: f.cert.Principal, App: "com.example.media", Service: id, ComponentType: sensorrecord.ComponentType, Stream: id}
}

func putMedia(t *testing.T, f *moduleReplicaFixture, source serviceaccess.Source, data []byte, mime string, until int64) serviceevidence.Reference {
	t.Helper()
	e := captureSchemaReference(data, mime)
	owner := f.m.owner(source)
	var ref serviceevidence.Reference
	for offset := 0; offset < len(data); offset += serviceevidence.MaxChunk {
		end := min(offset+serviceevidence.MaxChunk, len(data))
		r, err := f.m.evidence.Put(context.Background(), owner, serviceevidence.PutRequest{SHA256: e.SHA256, Size: e.Size, Offset: int64(offset), Data: data[offset:end], ContentType: mime, RetainUntilUnixMS: until})
		if err != nil {
			t.Fatal(err)
		}
		ref = r.Reference
	}
	return ref
}

func remoteReader(t *testing.T, f *moduleReplicaFixture) (serviceorigin.Scope, *binding) {
	t.Helper()
	f.publisher(t, 3, 1)
	scope := f.config.Policy.scope(f.config.Policy.Grants[3], 1)
	return scope, f.m.bindings[bindingKey(scope.App, scope.Service)]
}

func TestRemoteEvidencePreservesOwnerRetentionAndReopen(t *testing.T) {
	dials := 0
	f := newRemoteMediaFixture(t, func(node *moduleReplicaFixture, tr *ReplicaTransport) {
		original := tr.Dial
		tr.Dial = func(ctx context.Context, device string) (replication.Connection, error) {
			// Network work must not retain either host operation fence.
			if !node.identity.mu.TryLock() {
				t.Fatal("identity lock spans dial")
			}
			node.identity.mu.Unlock()
			if !node.m.replica.mu.TryLock() {
				t.Fatal("replica lock spans dial")
			}
			node.m.replica.mu.Unlock()
			dials++
			return original(ctx, device)
		}
	})
	scope, binding := remoteReader(t, f.a)
	source := mediaSource(f.b, "camera-b")
	data := bytes.Repeat([]byte("original-media"), 9000) // three bounded ranges
	want := putMedia(t, f.b, source, data, "image/png", moduleTestNow.Add(time.Minute).UnixMilli())
	got, raw, err := f.a.m.fetchRemoteEvidence(context.Background(), scope, source, want.SHA256, binding)
	if err != nil || got != want || !bytes.Equal(raw, data) || dials != 3 {
		t.Fatalf("original transfer: %v ranges=%d", err, dials)
	}
	store := f.a.m.evidenceStore(want.Owner)
	if err = retainRemoteEvidence(context.Background(), store, got, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.a.m.evidence.ReadComplete(context.Background(), want.Owner, want.SHA256, want.Size, want.ContentType); err == nil {
		t.Fatal("foreign owner became local")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	peer := f.a.m.replica.peers[f.b.cert.Principal]
	peer.evidence, err = serviceevidence.Open(serviceevidence.Config{Directory: peer.policy.EvidencePath, Tenant: want.Owner.Tenant, Site: want.Owner.Site, Device: want.Owner.Device, Clock: f.a.clock})
	if err != nil {
		t.Fatal(err)
	}
	reopened, raw, err := peer.evidence.ReadComplete(context.Background(), want.Owner, want.SHA256, want.Size, want.ContentType)
	if err != nil || reopened != want || !bytes.Equal(raw, data) {
		t.Fatal("cache reopen changed original", err)
	}
	longer := want
	longer.RetainUntilUnixMS++
	if retainRemoteEvidence(context.Background(), peer.evidence, longer, data) == nil {
		t.Fatal("repeated import extended retention")
	}
	f.a.clock.mu.Lock()
	f.a.clock.window.Earliest = moduleTestNow.Add(time.Minute)
	f.a.clock.window.Latest = moduleTestNow.Add(time.Minute + time.Millisecond)
	f.a.clock.window.Monotonic += time.Minute
	f.a.clock.mu.Unlock()
	if _, _, err = peer.evidence.ReadComplete(context.Background(), want.Owner, want.SHA256, want.Size, want.ContentType); err == nil {
		t.Fatal("expired retention became live")
	}
}

type mediaHookClient struct {
	replicav1.WorldReplicaServiceClient
	hook func(*replicav1.FetchEvidenceReply)
}

func (c mediaHookClient) FetchEvidence(ctx context.Context, q *replicav1.FetchEvidenceRequest, opts ...grpc.CallOption) (*replicav1.FetchEvidenceReply, error) {
	r, e := c.WorldReplicaServiceClient.FetchEvidence(ctx, q, opts...)
	if e == nil {
		c.hook(r)
	}
	return r, e
}

func TestRemoteEvidenceRejectsConflictingRangesAndWrongScope(t *testing.T) {
	for _, kind := range []string{"digest", "retention", "owner", "reader_replaced", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			f := newRemoteMediaFixture(t, func(node *moduleReplicaFixture, tr *ReplicaTransport) {
				original := tr.Dial
				tr.Dial = func(ctx context.Context, device string) (replication.Connection, error) {
					c, e := original(ctx, device)
					if e != nil {
						return c, e
					}
					c.Client = mediaHookClient{c.Client, func(r *replicav1.FetchEvidenceReply) {
						calls++
						if calls == 1 {
							switch kind {
							case "digest":
								r.Data[0] ^= 1
							case "owner":
								r.OwnerSha256 = strings.Repeat("a", 64)
							case "reader_replaced":
								node.m.mu.Lock()
								node.m.bindings[bindingKey("com.example.media", "reader")].scope.DeploymentGeneration++
								node.m.mu.Unlock()
							}
						}
						if calls == 2 && kind == "retention" {
							r.RetainUntilUnixMs++
						}
					}}
					return c, nil
				}
			})
			scope, b := remoteReader(t, f.a)
			source := mediaSource(f.b, "camera-b")
			data := bytes.Repeat([]byte("original"), 9000)
			want := putMedia(t, f.b, source, data, "image/png", moduleTestNow.Add(time.Minute).UnixMilli())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			_, raw, e := f.a.m.fetchRemoteEvidence(ctx, scope, source, want.SHA256, b)
			if e == nil || len(raw) != 0 {
				t.Fatal("inconsistent transfer escaped", kind, e)
			}
			if _, _, e = f.a.m.evidenceStore(want.Owner).ReadComplete(context.Background(), want.Owner, want.SHA256, want.Size, want.ContentType); e == nil {
				t.Fatal("failed fetch populated cache")
			}
			bad := source
			bad.Stream = "uncommissioned"
			if _, e = f.a.m.remoteEvidenceReader(context.Background(), scope, bad); e == nil {
				t.Fatal("uncommissioned remote source")
			}
		})
	}
}

func TestRemoteMediaDerivedAdmissionUsesCompleteOriginalCache(t *testing.T) {
	f := newRemoteMediaFixture(t, nil)
	scope, reader := remoteReader(t, f.a)
	fusion := f.a.publisher(t, 2, 1)
	var proofs []serviceorigin.SignedRecord
	var originals []serviceevidence.Reference
	for i, id := range []string{"camera-b", "audio-1"} {
		capture, data := fusionAppCapture(t, id, f.anchor)
		e, _ := capture.Evidence()
		ref := putMedia(t, f.b, mediaSource(f.b, id), data, e.ContentType, moduleTestNow.Add(time.Minute).UnixMilli())
		originals = append(originals, ref)
		p := f.b.publisher(t, i, 1)
		q, err := sensorrecord.Publication(capture, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		proofs = append(proofs, f.b.publish(t, p, q))
	}
	f.a.sync(t)
	claim, _ := (&remoteFusionFixture{a: f.a, b: f.b, fusion: fusion}).claim(t, proofs)
	if f.a.m.admitArchived(context.Background(), claim) == nil {
		t.Fatal("missing remote cache admitted")
	}
	for i, id := range []string{"camera-b", "audio-1"} {
		ref, data, err := f.a.m.fetchRemoteEvidence(context.Background(), scope, mediaSource(f.b, id), originals[i].SHA256, reader)
		if err != nil || ref != originals[i] {
			t.Fatal("remote evidence", err)
		}
		if err = retainRemoteEvidence(context.Background(), f.a.m.evidenceStore(ref.Owner), ref, data); err != nil {
			t.Fatal(err)
		}
	}
	var report preparedReport
	if err := f.a.m.admitArchivedReported(context.Background(), claim, &report); err != nil {
		t.Fatal("cached remote claim", err)
	}
	if report.inputVerifications != 2 || report.mediaReads != 2 || report.currentChecks < 1 || !report.closed {
		t.Fatalf("remote media did not use operation preparation: %+v", report)
	}
	for _, proof := range proofs {
		raw, _ := json.Marshal(proof)
		var record serviceorigin.Record
		_ = json.Unmarshal(proof.Record, &record)
		ref := exactReference(raw, serviceorigin.VerifiedRecord{Record: record, Bytes: proof.Record})
		stored, err := f.a.m.archive.Get(context.Background(), ref.Source(), ref)
		if err != nil || !bytes.Equal(stored.Envelope, raw) {
			t.Fatal("retained foreign proof changed", err)
		}
	}
	// A cached blob is not authority for a current observation.
	f.a.sample.Store(moduleTestNow.Add(4 * time.Second).UnixNano())
	if f.a.m.admitArchived(context.Background(), claim) == nil {
		t.Fatal("retained bytes extended input sample life")
	}
}

func TestRemoteEvidenceLinuxOrdinarySocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux peer credentials and pidfds")
	}
	f := newRemoteMediaFixture(t, nil)
	source := mediaSource(f.b, "camera-b")
	_, data := fusionAppCapture(t, "camera-b", f.anchor)
	ref := putMedia(t, f.b, source, data, "image/jpeg", moduleTestNow.Add(time.Minute).UnixMilli())
	g := f.a.config.Policy.Grants[3]
	mount, err := f.a.m.EnsureAccess(context.Background(), g.App, g.Service, 1, nil, g.Subscribe)
	if err != nil {
		t.Fatal(err)
	}
	client := moduleDial(t, mount.SocketPath)
	request := &appv1.GetEvidenceRequest{Source: accessSelector(source), Sha256: ref.SHA256, Length: serviceevidence.MaxChunk}
	got, err := client.GetEvidence(context.Background(), request)
	if err != nil || !bytes.Equal(got.Data, data) || got.RetainUntilUnixMs != ref.RetainUntilUnixMS {
		t.Fatal("ordinary remote read", err)
	}
	// A partition leaves the already retained historical bytes readable.
	f.b.policyOK.Store(false)
	got, err = client.GetEvidence(context.Background(), request)
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatal("cache required a second network fetch", err)
	}
	request.Source.Device = f.a.cert.Principal
	if _, err = client.GetEvidence(context.Background(), request); err == nil {
		t.Fatal("remote cache relabeled as local owner")
	}
	request.Source.Device = f.b.cert.Principal
	f.a.m.Release(g.App, g.Service, 1)
	if _, err = client.GetEvidence(context.Background(), request); err == nil {
		t.Fatal("released reader retained evidence access")
	}
}

func TestRemoteEvidenceCachePathsRemainExplicit(t *testing.T) {
	f := newRemoteMediaFixture(t, nil)
	for _, path := range []string{"relative", "/", f.a.config.JournalPath, f.a.config.EvidenceDirectory, f.a.config.Replication.AuthorityPath, f.a.config.Replication.Peers[0].RemotePath} {
		c := *f.a.config.Replication
		c.Peers = append([]ReplicaPeer(nil), c.Peers...)
		c.Peers[0].EvidencePath = path
		if c.validate(f.a.config.Policy, f.a.config.JournalPath, f.a.config.ProofArchivePath, f.a.config.EvidenceDirectory) == nil {
			t.Fatal("invalid cache path", path)
		}
	}
	peer := f.a.m.replica.peers[f.b.cert.Principal]
	store := peer.evidence
	peer.evidence = nil
	if f.a.m.evidenceStore(f.a.m.owner(mediaSource(f.b, "camera-b"))) != nil {
		t.Fatal("missing cache fell back to local")
	}
	peer.evidence = store
}
