package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func finalReadAwait(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("read did not reach the final blocking check")
	}
}

func finalReadAdvance(clock *moduleClock, d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.window.Earliest = clock.window.Earliest.Add(d)
	clock.window.Latest = clock.window.Latest.Add(d)
	clock.window.Monotonic += d
}

type finalReadResult struct {
	proofs []serviceorigin.SignedRecord
	err    error
}

// The already-verified seam permits a deterministic final-time test without
// manufacturing cryptographic authority. Real signed records are used below.
func TestFinalReadUsesOneCurrentTimeForWholeCaptureGroup(t *testing.T) {
	m, clock := captureSchemaManager(t)
	identity := &moduleIdentity{}
	identity.token.Store(1)
	m.deps.Identity = identity
	m.bindings = map[string]*binding{}
	sampleNow := time.Unix(0, m.cfg.CaptureSource.AnchorUnixNS+int64(250*time.Millisecond))
	m.deps.SampleNow = func() time.Time { return sampleNow }
	var proofs []serviceorigin.SignedRecord
	for i, name := range []string{"older", "newer"} {
		capture, _ := captureSchemaFixture(t, "gas")
		if i == 0 {
			capture.CapturedUnixNS -= int64(4 * time.Second)
		}
		v := captureSchemaRecord(t, m, capture)
		v.Record.Scope.Service = name
		v.Record.IssuedAtUnixMS = moduleTestNow.Add(-time.Minute).UnixMilli()
		v.Record.ExpiresAtUnixMS = moduleTestNow.Add(10 * time.Minute).UnixMilli()
		m.bindings[bindingKey(v.Record.Scope.App, name)] = &binding{scope: v.Record.Scope, epoch: v.Record.Epoch, token: 1}
		raw, err := json.Marshal(v.Record)
		if err != nil {
			t.Fatal(err)
		}
		proofs = append(proofs, serviceorigin.SignedRecord{Record: raw})
	}
	if out, err := m.filterCurrent(context.Background(), proofs); err != nil || len(out) != 2 {
		t.Fatalf("initial group is not live: count=%d, error=%v", len(out), err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	m.deps.SampleNow = func() time.Time { close(entered); <-resume; return sampleNow }
	done := make(chan finalReadResult, 1)
	go func() {
		out, err := m.filterCurrent(context.Background(), proofs)
		done <- finalReadResult{out, err}
	}()
	finalReadAwait(t, entered)
	finalReadAdvance(clock, 2*time.Second)
	unblock()
	select {
	case got := <-done:
		if got.err != nil || len(got.proofs) != 1 || !bytes.Equal(got.proofs[0].Record, proofs[1].Record) {
			t.Fatalf("earlier candidate escaped after expiring during later work: count=%d, error=%v", len(got.proofs), got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("final capture filtering did not finish")
	}
}

func TestFinalReadOmitsReleasedOrReplacedSource(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "release"
		if replacement {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			f := setupModule(t)
			first := modulePublish(t, moduleDial(t, f.mount(t, 0, 1).SocketPath), moduleRequest("gas"))
			secondRequest := moduleRequest("gas")
			secondRequest.Stream = "gas-b"
			second := modulePublish(t, moduleDial(t, f.mount(t, 1, 1).SocketPath), secondRequest)
			var firstProof, secondProof serviceorigin.SignedRecord
			if json.Unmarshal(first.SignedRecordJson, &firstProof) != nil || json.Unmarshal(second.SignedRecordJson, &secondProof) != nil {
				t.Fatal("actual broker returned invalid proof JSON")
			}
			proofs := []serviceorigin.SignedRecord{firstProof, secondProof}
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			t.Cleanup(unblock)
			f.manager.deps.SampleNow = func() time.Time { close(entered); <-resume; return moduleTestNow.Add(100 * time.Millisecond) }
			done := make(chan finalReadResult, 1)
			go func() {
				out, err := f.manager.filterCurrent(context.Background(), proofs)
				done <- finalReadResult{out, err}
			}()
			finalReadAwait(t, entered)
			if replacement {
				f.mount(t, 0, 2)
			} else {
				g := f.config.Policy.Grants[0]
				f.manager.Release(g.App, g.Service, 1)
			}
			unblock()
			select {
			case got := <-done:
				if got.err != nil || len(got.proofs) != 1 || !bytes.Equal(got.proofs[0].Record, secondProof.Record) {
					t.Fatalf("old source authority escaped final filtering: count=%d, error=%v", len(got.proofs), got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("source filtering did not finish")
			}
		})
	}
}

func TestFinalReadEvidenceExpiryDuringLastPeerCheckReturnsNoBytes(t *testing.T) {
	f := setupModule(t, accessConfig)
	source := accessSource("telemetry-a", "wendy.sensor.image.v1", "front")
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	reader := moduleDial(t, accessMount(t, f, source).SocketPath)
	data := []byte("private retained test bytes")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	deadline := moduleTestNow.Add(2 * time.Second).UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := writer.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: source.ComponentType, Stream: source.Stream, Sha256: digest, Size: int64(len(data)), Data: data, ContentType: "image/jpeg", RetainUntilUnixMs: deadline}); err != nil {
		t.Fatal(err)
	}
	get := &appv1.GetEvidenceRequest{Source: accessSelector(source), Sha256: digest, Length: int32(len(data))}
	if got, err := reader.GetEvidence(ctx, get); err != nil || !bytes.Equal(got.GetData(), data) {
		t.Fatalf("live evidence control failed: %v", err)
	}
	// The warmed connection has no handshake left. Access checks the peer on
	// RPC entry, before dispatch, at the fence, and finally before returning.
	original := f.manager.deps.PeerCheck
	entered, resume := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	f.manager.deps.PeerCheck = func(ctx context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
		if err := original(ctx, peer, scope); err != nil {
			return err
		}
		if scope.Service == "reader" && calls.Add(1) == 4 {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	type response struct {
		out *appv1.GetEvidenceReply
		err error
	}
	done := make(chan response, 1)
	go func() { out, err := reader.GetEvidence(ctx, get); done <- response{out, err} }()
	finalReadAwait(t, entered)
	finalReadAdvance(f.clock, 3*time.Second)
	unblock()
	select {
	case got := <-done:
		if got.err == nil || got.out != nil {
			t.Fatal("retained bytes escaped after expiring in the final peer check")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("evidence RPC did not finish")
	}
}

func TestFinalReadPhysicalClockRegressionLatches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*moduleClock)
	}{
		{"boot", func(c *moduleClock) { c.window.BootID = "different-boot" }},
		{"monotonic", func(c *moduleClock) { c.window.Monotonic-- }},
		{"earliest", func(c *moduleClock) { c.window.Earliest = c.window.Earliest.Add(-time.Nanosecond) }},
		{"latest", func(c *moduleClock) { c.window.Latest = c.window.Latest.Add(-time.Nanosecond) }},
		{"authentication", func(c *moduleClock) { c.window.Authenticated = false }},
		{"unavailable", func(c *moduleClock) { c.err = trustedtime.ErrUntrusted }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, clock := captureSchemaManager(t)
			initial, err := m.physicalNow()
			if err != nil {
				t.Fatal(err)
			}
			tc.change(clock)
			if _, err := m.physicalNow(); !errors.Is(err, ErrDenied) {
				t.Fatalf("regressed physical observation accepted: %v", err)
			}
			clock.window, clock.err = initial, nil
			if _, err := m.physicalNow(); !errors.Is(err, ErrDenied) || m.retentionResponseCurrent(serviceorigin.Scope{}, moduleTestNow.Add(time.Hour).UnixMilli()) {
				t.Fatal("clock recovery silently reopened response authority")
			}
		})
	}
}
