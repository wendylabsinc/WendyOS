package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofarchive"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	bolt "go.etcd.io/bbolt"
)

// This fixture uses actual issued identities, proof verification, media, archive
// and journal stores. It calls the host admission path directly and makes no
// claim about kernel peers; the separate Linux ordinary-app test covers those.
type preparedFixture struct {
	m                *Manager
	clock            *moduleClock
	identity         *moduleIdentity
	certificate      *testidentity.Fixture
	claim            serviceorigin.SignedRecord
	verified         serviceorigin.VerifiedRecord
	descriptors      []proofarchive.Descriptor
	claimPublisher   appservice.Publisher
	sourcePublishers []appservice.Publisher
	sourceRequests   []serviceorigin.PublishRequest
	request          serviceorigin.PublishRequest
	sample           atomic.Int64
	calls            atomic.Int64
	hookMu           sync.Mutex
	hook             func(int64)
}

func newPreparedFixture(t *testing.T) *preparedFixture {
	t.Helper()
	f := &preparedFixture{clock: moduleTestClock()}
	f.sample.Store(moduleTestNow.Add(100 * time.Millisecond).UnixNano())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy := basePolicy()
	policy.ExecutionDomain, policy.SampleClockDomain = "simulation", "simulation:run-a"
	policy.Grants = nil
	var sources []serviceaccess.Source
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		source := serviceaccess.Source{App: "com.example.prepared", Service: "source-" + id, ComponentType: sensorrecord.ComponentType, Stream: id}
		sources = append(sources, source)
		policy.Grants = append(policy.Grants, Grant{App: source.App, Service: source.Service, Publish: []serviceorigin.PublishScope{{ComponentType: source.ComponentType, Stream: source.Stream}}})
	}
	policy.Grants = append(policy.Grants, Grant{App: "com.example.prepared", Service: "fusion", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "result"}}, Subscribe: sources})
	cert := testidentity.NewWith(t, moduleTestNow, policy.Device)
	f.certificate = cert
	f.identity = newModuleIdentity(cert)
	anchor := moduleTestNow.Add(-150 * time.Millisecond).UnixNano()
	cfg := Config{Policy: policy, Roots: [][]byte{cert.RootDER}, Directory: filepath.Join(base, "sockets"), JournalPath: filepath.Join(base, "journal", "world.db"), ProofArchivePath: filepath.Join(base, "archive", "proofs.db"), EvidenceDirectory: filepath.Join(base, "media"), CreateJournal: true, CreateProofArchive: true, UID: os.Geteuid(), GID: os.Getegid(), CaptureSource: &CaptureSource{RunID: "run-a", BootID: "boot-original", PackageSHA256: string(bytes.Repeat([]byte("a"), 64)), AnchorUnixNS: anchor, Frame: policy.Frames[0]}}
	deps := Dependencies{Identity: f.identity, Clock: f.clock, PolicyCurrent: func(context.Context) error { return nil }, PeerCheck: func(context.Context, appservice.Peer, serviceorigin.Scope) error { return ErrDenied }, SampleNow: func() time.Time {
		n := f.calls.Add(1)
		f.hookMu.Lock()
		hook := f.hook
		f.hookMu.Unlock()
		if hook != nil {
			hook(n)
		}
		return time.Unix(0, f.sample.Load())
	}}
	f.m, err = New(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.m.Close() })
	var refs []proofref.Reference
	var provenance []world.Input
	for i, source := range sources {
		publisher := f.publisher(t, i)
		f.sourcePublishers = append(f.sourcePublishers, publisher)
		capture, data := fusionAppCapture(t, source.Stream, anchor)
		publication, e := sensorrecord.Publication(capture, 3*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		f.sourceRequests = append(f.sourceRequests, publication)
		ref, e := capture.Evidence()
		if e != nil {
			t.Fatal(e)
		}
		if ref != nil {
			_, e = f.m.evidence.Put(context.Background(), f.m.owner(source), serviceevidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, Data: data, ContentType: ref.ContentType, RetainUntilUnixMS: moduleTestNow.Add(2 * time.Minute).UnixMilli()})
			if e != nil {
				t.Fatal(e)
			}
		}
		signed, e := publisher.Publish(context.Background(), publication)
		if e != nil {
			t.Fatal(e)
		}
		if e = f.m.admitArchived(context.Background(), signed); e != nil {
			t.Fatal(e)
		}
		v, e := f.m.verifier.Verify(context.Background(), signed)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(signed)
		exact := exactReference(raw, v)
		stored, e := f.m.archive.Get(context.Background(), exact.Source(), exact)
		if e != nil {
			t.Fatal(e)
		}
		f.descriptors = append(f.descriptors, stored.Descriptor)
		refs = append(refs, exact)
		provenance = append(provenance, world.Input{ID: exact.ID(), Origin: world.Simulated, Simulation: publication.Provenance.Simulation})
	}
	payload, err := resultrecord.Encode(resultrecord.Result{Version: 1, ResultSchema: "example.prepared.v1", ResultJSON: []byte("{ \"label\": \"gas elevated\" }\n"), Inputs: refs, RetainUntilUnixMS: moduleTestNow.Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	f.request = serviceorigin.PublishRequest{Entity: "prepared-result", ComponentType: resultrecord.ComponentType, Stream: "result", Frame: policy.Frames[0], Timing: world.Timing{SampleTime: moduleTestNow.Add(-150 * time.Millisecond), ClockDomain: policy.SampleClockDomain, ErrorKnown: true}, TTL: 3 * time.Second, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "test", ModelID: "rule", ModelVersion: "1", Inputs: provenance}, Payload: payload}
	f.claimPublisher = f.publisher(t, 4)
	f.claim, err = f.claimPublisher.Publish(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.verified, err = f.m.verifier.Verify(context.Background(), f.claim)
	if err != nil {
		t.Fatal(err)
	}
	f.calls.Store(0)
	return f
}
func (f *preparedFixture) publisher(t *testing.T, index int) appservice.Publisher {
	t.Helper()
	g := f.m.cfg.Policy.Grants[index]
	scope := f.m.cfg.Policy.scope(g, uint64(index+1))
	f.m.mu.Lock()
	f.m.bindings[bindingKey(g.App, g.Service)] = &binding{scope: scope, subscribe: append([]serviceaccess.Source(nil), g.Subscribe...)}
	f.m.mu.Unlock()
	p, e := f.m.publisher(context.Background(), scope)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func (f *preparedFixture) setHook(h func(int64)) { f.hookMu.Lock(); f.hook = h; f.hookMu.Unlock() }
func (f *preparedFixture) prepare(t *testing.T) (context.Context, *preparedPublication) {
	t.Helper()
	raw, _ := json.Marshal(f.claim)
	ctx, p, e := f.m.preparePublication(context.Background(), raw)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.close)
	if _, e = f.m.derivedFromDescriptors(ctx, f.verified, f.descriptors); e != nil {
		t.Fatal(e)
	}
	return ctx, p
}

func TestPreparedActualAdmissionReadsEachMediaOnceAndCannotReuseAcrossCalls(t *testing.T) {
	f := newPreparedFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			var e error
			f.claim, e = f.claimPublisher.Publish(context.Background(), f.request)
			if e != nil {
				t.Fatal(e)
			}
		}
		f.calls.Store(0)
		var report preparedReport
		if e := f.m.admitArchivedReported(context.Background(), f.claim, &report); e != nil {
			t.Fatal(e)
		}
		if report.inputVerifications != 4 || report.mediaReads != 3 || report.currentChecks != 9 || !report.closed || f.calls.Load() != 14 {
			t.Fatalf("actual callbacks/read counts: %+v sample_reads=%d", report, f.calls.Load())
		}
		raw, _ := json.Marshal(f.claim)
		v, e := f.m.verifier.Verify(context.Background(), f.claim)
		if e != nil {
			t.Fatal(e)
		}
		ref := exactReference(raw, v)
		stored, e := f.m.archive.Get(context.Background(), ref.Source(), ref)
		if e != nil || !bytes.Equal(raw, stored.Envelope) {
			t.Fatal("exact admitted result proof lost", e)
		}
		t.Logf("actual admission: %d input verifications, %d complete media reads, %d later fresh validation callbacks, %d source observations", report.inputVerifications, report.mediaReads, report.currentChecks, f.calls.Load())
	}
}

func TestPreparedRejectsWrongOperationClaimDescriptorsAndCanceledHandle(t *testing.T) {
	f := newPreparedFixture(t)
	ctx, p := f.prepare(t)
	other := &Manager{}
	if _, e := p.current(ctx, other, f.verified); e == nil {
		t.Fatal("different manager accepted")
	}
	if _, e := p.current(context.Background(), f.m, f.verified); e == nil {
		t.Fatal("different operation accepted")
	}
	changed := f.verified
	changed.Bytes = bytes.Clone(changed.Bytes)
	changed.Bytes[0] ^= 1
	if _, e := p.current(ctx, f.m, changed); e == nil {
		t.Fatal("changed record accepted")
	}
	changed = f.verified
	changed.Record.Publication.Entity = "different"
	if _, e := p.current(ctx, f.m, changed); e == nil {
		t.Fatal("changed projection accepted")
	}
	for _, change := range []func([]proofarchive.Descriptor){
		func(d []proofarchive.Descriptor) { d[0], d[1] = d[1], d[0] },
		func(d []proofarchive.Descriptor) { d[0].Reference.Version.Sequence++ },
		func(d []proofarchive.Descriptor) { d[0].Envelope = bytes.Clone(d[0].Envelope); d[0].Envelope[0] ^= 1 },
		func(d []proofarchive.Descriptor) { d[0].RetainUntilUnixMS-- },
	} {
		d := append([]proofarchive.Descriptor(nil), f.descriptors...)
		change(d)
		if _, e := f.m.derivedFromDescriptors(ctx, f.verified, d); e == nil {
			t.Fatal("changed descriptor accepted")
		}
	}
	// Mutation of an original descriptor buffer cannot rewrite the owned copy.
	f.descriptors[0].Envelope[0] ^= 1
	if _, e := p.current(ctx, f.m, f.verified); e != nil {
		t.Fatal("caller descriptor changed prepared facts", e)
	}
	p.close()
	if _, e := p.current(ctx, f.m, f.verified); e == nil {
		t.Fatal("finished operation reused")
	}
}

func TestPreparedFreshAuthorityAtEveryBlockingSourceObservation(t *testing.T) {
	for _, boundary := range []int64{2, 3, 4, 5, 9, 10, 11, 12, 13, 14} {
		for _, fault := range []string{"source_epoch", "source_generation", "read_grant", "policy", "identity", "sample_expiry", "physical_expiry", "policy_window"} {
			t.Run(fmtBoundary(boundary, fault), func(t *testing.T) {
				f := newPreparedFixture(t)
				f.setHook(func(n int64) {
					if n != boundary {
						return
					}
					f.m.mu.Lock()
					defer f.m.mu.Unlock()
					source := f.m.bindings[bindingKey("com.example.prepared", "source-camera-a")]
					switch fault {
					case "source_epoch":
						source.epoch++
					case "source_generation":
						source.scope.DeploymentGeneration++
					case "read_grant":
						f.m.bindings[bindingKey("com.example.prepared", "fusion")].subscribe = nil
					case "policy":
						f.m.policyFailed.Store(true)
					case "identity":
						f.identity.token.Store(0)
					case "sample_expiry":
						f.sample.Store(moduleTestNow.Add(4 * time.Second).UnixNano())
					case "physical_expiry":
						f.clock.mu.Lock()
						f.clock.window.Earliest = f.clock.window.Earliest.Add(6 * time.Second)
						f.clock.window.Latest = f.clock.window.Latest.Add(6 * time.Second)
						f.clock.window.Monotonic += 6 * time.Second
						f.clock.mu.Unlock()
					case "policy_window":
						f.clock.mu.Lock()
						f.clock.window.Latest = f.m.cfg.Policy.NotAfter
						f.clock.mu.Unlock()
					}
				})
				e := f.m.admitArchived(context.Background(), f.claim)
				if e == nil {
					t.Fatal("late authority/deadline fault acknowledged")
				}
				if boundary >= 10 && !errors.Is(e, proofarchive.ErrUncertain) {
					t.Fatalf("postcommit error lost uncertain semantics: %v", e)
				}
			})
		}
	}
}

func fmtBoundary(n int64, fault string) string { return time.Duration(n).String() + "/" + fault }

func TestPreparedCancellationAfterJournalCommitAndConcurrentReplacement(t *testing.T) {
	for _, mode := range []string{"cancel_after_commit", "replace_during_wait"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			boundary := int64(2)
			if mode == "cancel_after_commit" {
				boundary = 10
			}
			f.setHook(func(n int64) {
				if n == boundary {
					close(entered)
					<-release
				}
			})
			finished := make(chan error, 1)
			go func() { finished <- f.m.admitArchived(ctx, f.claim) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("source checkpoint not reached")
			}
			if mode == "cancel_after_commit" {
				cancel()
			} else {
				f.m.mu.Lock()
				f.m.bindings[bindingKey("com.example.prepared", "source-camera-a")].scope.DeploymentGeneration++
				f.m.mu.Unlock()
			}
			close(release)
			select {
			case e := <-finished:
				if e == nil {
					t.Fatal("concurrent fault acknowledged")
				}
				if mode == "cancel_after_commit" && !errors.Is(e, proofarchive.ErrUncertain) {
					t.Fatal("commit uncertainty lost", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("admission deadlocked")
			}
			if mode == "cancel_after_commit" {
				if e := f.m.journal.Close(); e != nil {
					t.Fatal(e)
				}
				db, e := bolt.Open(f.m.cfg.JournalPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
				if e != nil {
					t.Fatal(e)
				}
				defer db.Close()
				want, _ := json.Marshal(f.claim)
				found := false
				e = db.View(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte("proofs")).ForEach(func(_, raw []byte) error {
						var stored struct {
							Envelope []byte `json:"envelope"`
						}
						if e := json.Unmarshal(raw, &stored); e != nil {
							return e
						}
						if bytes.Equal(want, stored.Envelope) {
							found = true
						}
						return nil
					})
				})
				if e != nil || !found {
					t.Fatal("canceled postcommit outcome did not preserve exact durable proof", e)
				}
			}
		})
	}
}

func TestPreparedInitialCorruptMediaIsNeverAdmitted(t *testing.T) {
	f := newPreparedFixture(t)
	var v serviceorigin.SignedRecord
	if e := json.Unmarshal(f.descriptors[0].Envelope, &v); e != nil {
		t.Fatal(e)
	}
	verified, e := f.m.verifier.Verify(context.Background(), v)
	if e != nil {
		t.Fatal(e)
	}
	c, e := sensorrecord.FromComponent(verified.Component())
	if e != nil {
		t.Fatal(e)
	}
	ref, e := c.Evidence()
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(f.m.cfg.EvidenceDirectory, "blobs", ref.SHA256+".blob"), bytes.Repeat([]byte{0}, int(ref.Size)), 0600); e != nil {
		t.Fatal(e)
	}
	if e = f.m.admitArchived(context.Background(), f.claim); e == nil {
		t.Fatal("corrupt initial content admitted")
	}
}

func TestPreparedConcurrentRawAdmissionAndSnapshotDoNotDeadlock(t *testing.T) {
	f := newPreparedFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.setHook(func(n int64) {
		if n == 2 {
			close(entered)
			<-release
		}
	})
	claimDone := make(chan error, 1)
	go func() { claimDone <- f.m.admitArchived(context.Background(), f.claim) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pin checkpoint not reached")
	}
	// A newer raw version keeps the original same-epoch input valid. Its
	// admission waits on archive serialization while an independent replica
	// read also exercises the journal's lock order.
	raw, err := f.sourcePublishers[2].Publish(context.Background(), f.sourceRequests[2])
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	rawDone, readDone := make(chan error, 1), make(chan error, 1)
	go func() { rawDone <- f.m.admitArchived(context.Background(), raw) }()
	go func() { _, e := f.m.Snapshot(context.Background()); readDone <- e }()
	close(release)
	for _, done := range []chan error{claimDone, rawDone, readDone} {
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cross-store operation deadlocked")
		}
	}
}
