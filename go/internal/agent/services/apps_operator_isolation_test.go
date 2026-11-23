package services

// Isolation tier: the real Agent postbox, signature verifier, crypto, ordering
// and recovery code run with synthetic in-process registry/runtime peers.
// These tests establish those boundaries, not live Cloud or containerd coverage.
// Barriers exist only in this test file; production code has no new hooks.
import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func appsIsolationOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("WENDY_E2E_APPS_ISOLATION") != "true" {
		t.Skip("explicit isolated Agent fixture opt-in required")
	}
}

type appsBlockedArtifacts struct {
	*fakeArtifacts
	entered chan struct{}
	once    sync.Once
}

func (a *appsBlockedArtifacts) GetBlob(ctx context.Context, _ *pb.GetBlobRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.GetBlobResponse], error) {
	a.once.Do(func() { close(a.entered) })
	<-ctx.Done()
	return nil, status.Error(codes.Canceled, "isolated blob barrier canceled")
}
func TestAppsOperatorIsolationRemovalDuringPull(t *testing.T) {
	appsIsolationOptIn(t)
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	barrier := &appsBlockedArtifacts{fakeArtifacts: rec.arts, entered: make(chan struct{})}
	f.p.dialArtifacts = func(string, string, string, string) (pb.DeviceArtifactServiceClient, io.Closer, error) {
		return barrier, io.NopCloser(nil), nil
	}
	f.assignEntry(r.assign)
	f.cycleWithin(5 * time.Second)
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not reach its deterministic blob barrier")
	}
	f.unassignEntry(r.assign.DeploymentId)
	f.cycleWithin(5 * time.Second)
	f.p.waitPulls()
	if len(rec.imported) != 0 || len(rec.starts) != 0 {
		t.Fatal("removed pull reached import or start")
	}
	if mark := f.mark(r.assign.DeploymentId); mark == nil || !mark.Removed || mark.Revision != maxRevision {
		t.Fatal("terminal removal mark missing")
	}
	// Reload durable state and deliver an older legitimate synthetic assignment.
	f.p.mu.Lock()
	f.p.state = nil
	f.p.mu.Unlock()
	f.assignEntry(r.assign)
	f.cycle()
	if f.held(r.assign.DeploymentId) != nil || len(rec.starts) != 0 {
		t.Fatal("late assignment resurrected a terminally removed app")
	}
	if f.outcome("web").Outcome != pb.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE {
		t.Fatal("late assignment did not report stale refusal")
	}
}

type appsStartBarrier struct {
	*importRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *appsStartBarrier) StartRelease(ctx context.Context, s ReleaseStart) (bool, error) {
	if slices.Contains(s.Env, "MODE=b") {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return b.importRecorder.StartRelease(ctx, s)
}
func TestAppsOperatorIsolationOverlappingStarts(t *testing.T) {
	appsIsolationOptIn(t)
	f, rec, base := runningRelease(t)
	barrier := &appsStartBarrier{importRecorder: rec, entered: make(chan struct{}), release: make(chan struct{})}
	f.p.control = barrier
	rec.startErr = func(s ReleaseStart) error {
		if slices.Contains(s.Env, "MODE=b") || slices.Contains(s.Env, "MODE=c") {
			return errors.New("isolated declared startup failure")
		}
		return nil
	}
	b := proto.Clone(base).(*pb.DeviceAssignment)
	b.Revision = 2
	b.ConfigJson = `{"MODE":"b"}`
	f.assignEntry(b)
	f.cycleWithin(5 * time.Second)
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach start barrier")
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(barrier.release)
		}
	})
	c := proto.Clone(base).(*pb.DeviceAssignment)
	c.Revision = 3
	c.ConfigJson = `{"MODE":"c"}`
	f.assignEntry(c)
	done := make(chan error, 1)
	go func() { done <- f.p.cycle(context.Background()) }()
	// The mark is committed before canceling the previous pull. Observe that
	// real transition, then release B; do not race with a wall-clock sleep.
	waitUntil(t, "revision three was accepted while revision two was starting", func() bool { m := f.mark(base.DeploymentId); return m != nil && m.Revision == 3 })
	close(barrier.release)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("newer revision did not settle")
	}
	f.p.waitPulls()
	if rec.app == nil || !rec.app.running || !slices.Contains(rec.app.env, "MODE=a") {
		t.Fatal("overlapping failed replacements did not preserve last applied configuration A")
	}
	if f.outcome("web").Outcome != pb.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED {
		t.Fatal("newer failed replacement did not report failure")
	}
}

func TestAppsOperatorIsolationBadDataRefused(t *testing.T) {
	appsIsolationOptIn(t)
	for _, kind := range []string{"manifest", "blob", "wrap", "revision-zero", "wrong-target", "bad-signature"} {
		t.Run(kind, func(t *testing.T) {
			f, rec, base := runningRelease(t)
			r := rec.arts.r
			rec.present = nil // force actual cryptographic delivery checks
			next := proto.Clone(base).(*pb.DeviceAssignment)
			next.Revision = 2
			want := pb.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST
			switch kind {
			case "manifest":
				r.material.Manifest.OciManifest = append(slices.Clone(r.material.Manifest.OciManifest), ' ')
			case "blob":
				b := r.material.Manifest.Blobs[1]
				r.cipher[b.CiphertextDigest] = slices.Clone(r.cipher[b.CiphertextDigest])
				r.cipher[b.CiphertextDigest][0] ^= 1
			case "wrap":
				next.DeviceWrap.Ciphertext = slices.Clone(next.DeviceWrap.Ciphertext)
				next.DeviceWrap.Ciphertext[0] ^= 1
				want = pb.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP
			case "revision-zero":
				next.Revision = 0
				want = pb.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE
			case "wrong-target":
				next.Device = "spiffe://wendy.sh/tenant/" + f.tenant + "/device/unrelated-isolated-device"
				want = pb.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET
			}
			if kind == "bad-signature" {
				resource := "org/" + f.tenant + "/deployment/" + next.DeploymentId + "/device/" + f.uuid
				jws, payload, typ := f.sign(assignNew, next, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
				// Synthetic fixture bytes only, never transmitted to a service.
				at := len(jws) - 12
				if jws[at] == 'A' {
					jws[at] = 'B'
				} else {
					jws[at] = 'A'
				}
				f.cloud.add(jws, payload, typ)
			} else {
				f.assignEntry(next)
			}
			beforeImports, beforeStarts := len(rec.imported), len(rec.starts)
			f.cycle()
			if kind == "bad-signature" {
				if f.p.refusals["signature"] != 1 {
					t.Fatal("synthetic invalid signature was not refused")
				}
			} else if f.outcome("web").Outcome != want {
				t.Fatalf("%s outcome %s, want %s", kind, f.outcome("web").Outcome, want)
			}
			if len(rec.imported) != beforeImports || len(rec.starts) != beforeStarts {
				t.Fatal("synthetic invalid data reached runtime operations")
			}
			if rec.app == nil || !rec.app.running || !slices.Contains(rec.app.env, "MODE=a") {
				t.Fatal("invalid delivery displaced the previous working app")
			}
		})
	}
}
