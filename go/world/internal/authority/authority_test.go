package authority

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type fixture struct {
	now       time.Time
	root, key ed25519.PrivateKey
	grant     SignedGrant
	verifier  *Verifier
	rev       SignedRevocations
}

func newFixture(t *testing.T, replay ReplayStore) *fixture {
	t.Helper()
	rootPub, root, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	f := &fixture{now: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), root: root, key: key}
	g := Grant{PublishAudience: "node-1", Version: Version, ID: "grant-1", Tenant: "tenant", Site: "office", Domain: "simulation", Principal: "sensor", Kind: "app", Source: world.Source{Device: "gateway", App: "sensors", Service: "camera", Stream: "north"}, PublicKey: pub, Scopes: []string{"publish", "read", "replicate"}, NotBeforeUnixMS: f.now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: f.now.Add(time.Hour).UnixMilli()}
	var err error
	f.grant, err = SignGrant(g, root)
	if err != nil {
		t.Fatal(err)
	}
	if replay == nil {
		replay = NewMemoryReplay(1000)
	}
	f.verifier, err = NewVerifier(Config{RootPublicKey: rootPub, Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "node-1", Now: func() time.Time { return f.now }, Replay: replay})
	if err != nil {
		t.Fatal(err)
	}
	f.rev, err = SignRevocations(Revocations{Version: Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: f.now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: f.now.Add(59 * time.Minute).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(f.rev); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) request(t *testing.T, op, nonce string, body []byte) SignedRequest {
	t.Helper()
	r, err := SignRequest(f.grant, f.key, "node-1", op, body, nonce, f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestSignedRequestBindsIdentityScopeBodyAndAudience(t *testing.T) {
	f := newFixture(t, nil)
	body := []byte(`{"value":3}`)
	r := f.request(t, "publish", "nonce_1234567890", body)
	for _, change := range []func(*SignedRequest){func(r *SignedRequest) { r.Audience = "other" }, func(r *SignedRequest) { r.Operation = "stop" }, func(r *SignedRequest) { r.Grant.Grant.Source.Service = "other" }, func(r *SignedRequest) { r.Signature[0] ^= 1 }, func(r *SignedRequest) { r.Grant.Grant.Site = "other" }, func(r *SignedRequest) { r.Grant.Grant.Domain = "physical" }} {
		b, _ := json.Marshal(r)
		var copy SignedRequest
		_ = json.Unmarshal(b, &copy)
		change(&copy)
		if _, err := f.verifier.Verify(copy, "publish", body); err == nil {
			t.Fatal("tamper accepted")
		}
	}
	if _, err := f.verifier.Verify(r, "publish", []byte(`{"value":4}`)); err == nil {
		t.Fatal("changed body accepted")
	}
	id, err := f.verifier.Verify(r, "publish", body)
	if err != nil {
		t.Fatal(err)
	}
	if id.Source.Service != "camera" || !id.Allows("publish") || id.Principal != "sensor" {
		t.Fatalf("wrong identity: %+v", id)
	}
	if _, err := f.verifier.Verify(r, "publish", body); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: %v", err)
	}
}
func TestExpiryWrongRootAndMissingScope(t *testing.T) {
	f := newFixture(t, nil)
	r := f.request(t, "mission", "nonce_1234567890", nil)
	if _, err := f.verifier.Verify(r, "mission", nil); err == nil {
		t.Fatal("scope bypass")
	}
	r = f.request(t, "read", "nonce_1234567891", nil)
	f.now = f.now.Add(time.Minute)
	if _, err := f.verifier.Verify(r, "read", nil); err == nil {
		t.Fatal("expired request")
	}
	f = newFixture(t, nil)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.verifier.cfg.RootPublicKey = other
	if _, err := f.verifier.Verify(f.request(t, "read", "nonce_1234567892", nil), "read", nil); err == nil {
		t.Fatal("wrong root")
	}
}
func TestAgentCannotReceiveMotionOrStopGrant(t *testing.T) {
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "agent"
	for _, scope := range []string{"mission", "stop"} {
		g.Scopes = []string{scope}
		if _, err := SignGrant(g, f.root); err == nil {
			t.Fatalf("agent given %s", scope)
		}
	}
	g.Scopes = []string{"propose"}
	if _, err := SignGrant(g, f.root); err != nil {
		t.Fatal(err)
	}
}
func TestRevocationFreshnessRollbackAndTamper(t *testing.T) {
	f := newFixture(t, nil)
	r := f.request(t, "read", "nonce_1234567890", nil)
	rev := f.rev.Revocations
	rev.Revision = 2
	rev.KeyIDs = []string{KeyID(f.grant.Grant.PublicKey)}
	signed, _ := SignRevocations(rev, f.root)
	if err := f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.Verify(r, "read", nil); err == nil {
		t.Fatal("revoked key accepted")
	}
	if err := f.verifier.UpdateRevocations(f.rev); err == nil {
		t.Fatal("revocation rollback")
	}
	signed.Revocations.Revision = 3
	if err := f.verifier.UpdateRevocations(signed); err == nil {
		t.Fatal("tampered denylist")
	}
	f = newFixture(t, nil)
	f.now = f.now.Add(59 * time.Minute)
	if _, err := f.verifier.Verify(f.request(t, "read", "nonce_1234567890", nil), "read", nil); !errors.Is(err, ErrStaleRevocations) {
		t.Fatalf("stale denylist: %v", err)
	}
}

func TestValidateRevocationsIsReadOnlyAndStillChecksDurableRevision(t *testing.T) {
	f := newFixture(t, nil)
	r := f.rev.Revocations
	r.Revision = 3
	signed, _ := SignRevocations(r, f.root)
	if err := f.verifier.ValidateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if f.verifier.rev.Revocations.Revision != 1 {
		t.Fatal("read-only validation installed snapshot")
	}
	r.Revision = 2
	second, _ := SignRevocations(r, f.root)
	if err := f.verifier.UpdateRevocations(second); err != nil {
		t.Fatal("read-only validation consumed revision", err)
	}
	if err := f.verifier.ValidateRevocations(f.rev); err == nil {
		t.Fatal("read-only validation ignored durable revision")
	}
}
func TestExpiredOperatorRoleCanStopButRevokedCannot(t *testing.T) {
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"stop", "mission"}
	g.NotAfterUnixMS = f.now.Add(time.Second).UnixMilli()
	f.grant, _ = SignGrant(g, f.root)
	f.now = f.now.Add(2 * time.Second)
	if _, err := f.verifier.Verify(f.request(t, "stop", "nonce_1234567890", nil), "stop", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.Verify(f.request(t, "mission", "nonce_1234567891", nil), "mission", nil); err == nil {
		t.Fatal("expired motion grant accepted")
	}
	rev := f.rev.Revocations
	rev.Revision = 2
	rev.Principals = []string{g.Principal}
	signed, _ := SignRevocations(rev, f.root)
	if err := f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.Verify(f.request(t, "stop", "nonce_1234567892", nil), "stop", nil); err == nil {
		t.Fatal("revoked stop accepted")
	}
}
func TestRecordsAuthenticateOriginAndPermitHighWaterRepair(t *testing.T) {
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.NotAfterUnixMS = f.now.Add(time.Second).UnixMilli()
	f.grant, _ = SignGrant(g, f.root)
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Entity: "person", Type: "pose", Source: g.Source}, Version: world.Version{Epoch: 1, Sequence: 1}, TTL: time.Second, Payload: json.RawMessage(`{"x":1}`)}
	r, err := SignRecord(f.grant, f.key, c, f.now)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Second) // component AND grant expired, proof remains within repair horizon
	if _, err = f.verifier.VerifyRecord(r); err != nil {
		t.Fatalf("historical repair denied: %v", err)
	}
	r.Component.Timing.AgeHint = time.Second
	if _, err = f.verifier.VerifyRecord(r); err != nil {
		t.Fatal("unsigned age hint cannot break proof", err)
	}
	r.Component.Payload = json.RawMessage(`{"x":2}`)
	if _, err = f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("changed record accepted")
	}
	c.Tombstone = true
	c.Payload = nil
	r, _ = SignRecord(f.grant, f.key, c, f.now.Add(-2*time.Second))
	if _, err = f.verifier.VerifyRecord(r); err != nil {
		t.Fatal("expired tombstone proof rejected", err)
	}
	r.Component.Key.Source.Service = "fake"
	if _, err = f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("forged source accepted")
	}
}
func TestRecordDeadlinesAndDomainIsolation(t *testing.T) {
	f := newFixture(t, nil)
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Source: f.grant.Grant.Source}, TTL: time.Second}
	r, _ := SignRecord(f.grant, f.key, c, f.now.Add(time.Hour))
	if _, err := f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("future record accepted")
	}
	r, _ = SignRecord(f.grant, f.key, c, f.now)
	r.ExpiresAtUnixMS++
	if _, err := f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("changed deadline accepted")
	}
	f.verifier.cfg.Domain = "physical"
	if _, err := f.verifier.VerifyRecord(r); err == nil {
		t.Fatal("simulation grant admitted by physical verifier")
	}
}
func TestStrictEnvelopeDecoding(t *testing.T) {
	f := newFixture(t, nil)
	r := f.request(t, "read", "nonce_1234567890", nil)
	b, _ := json.Marshal(r)
	if _, err := DecodeRequest(b); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(append([]byte(nil), b...), []byte(` {}`)...), []byte(`{"version":1,"version":1}`), []byte(`{"unknown":1}`), make([]byte, MaxEnvelopeBytes+1)} {
		if _, err := DecodeRequest(bad); err == nil {
			t.Fatal("bad envelope accepted")
		}
	}
}
func TestDurableReplayAndRevocationCheckpointSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	cache, err := OpenReplay(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, cache)
	r := f.request(t, "read", "nonce_1234567890", nil)
	if _, err = f.verifier.Verify(r, "read", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenReplay(path, 10); err == nil {
		t.Fatal("concurrent process open accepted")
	}
	if err = cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = OpenReplay(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	f.verifier.cfg.Replay = cache
	if _, err = f.verifier.Verify(r, "read", nil); !errors.Is(err, ErrReplay) {
		t.Fatalf("restart replay: %v", err)
	}
	if err = cache.Checkpoint(1, "different", f.now); err == nil {
		t.Fatal("same revision conflict")
	}
	if err = cache.Checkpoint(2, "new", f.now); err != nil {
		t.Fatal(err)
	}
	if err = cache.Checkpoint(1, "old", f.now); err == nil {
		t.Fatal("revision rollback")
	}
	f.now = f.now.Add(-time.Second)
	if _, err = f.verifier.Verify(f.request(t, "read", "nonce_1234567891", nil), "read", nil); err == nil {
		t.Fatal("clock rollback")
	}
}
func TestConcurrentReplayHasSingleWinner(t *testing.T) {
	c := NewMemoryReplay(20)
	now := time.Now()
	var winners atomic.Int32
	var wg sync.WaitGroup
	attempt := func() {
		defer wg.Done()
		if c.Consume("one", now.Add(time.Minute), now) == nil {
			winners.Add(1)
		}
	}
	wg.Add(2)
	go attempt()
	go attempt()
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d accepted", winners.Load())
	}
}
func TestReplayCapacityWriteFailureAndMalformedState(t *testing.T) {
	now := time.Now()
	c := NewMemoryReplay(1)
	if err := c.Consume("one", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := c.Consume("two", now.Add(time.Minute), now); err == nil {
		t.Fatal("capacity exceeded")
	}
	path := filepath.Join(t.TempDir(), "ledger")
	d, err := OpenReplay(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Destination directory makes rename fail after temp-file fsync; no nonce commits.
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = d.Consume("failed", now.Add(time.Minute), now); err == nil {
		t.Fatal("write failure ignored")
	}
	if len(d.state.Nonces) != 0 {
		t.Fatal("failed write changed memory")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = d.Consume("failed", now.Add(time.Minute), now); err != nil {
		t.Fatal("failed nonce was consumed", err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte(`{"version":`), 0600)
	if _, err = OpenReplay(bad, 10); err == nil {
		t.Fatal("truncated ledger accepted")
	}
}
