package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnnotationClockClientPinsBoundsAndRequestTime(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/native-annotation-clock.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Credentials Config                            `json:"credentials"`
		Observation annotationclock.SignedObservation `json:"observation"`
		Revocations authority.SignedRevocations       `json:"revocations"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	var grant authority.SignedGrant
	if err = json.Unmarshal(fixture.Observation.Grant, &grant); err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 64)
	}
	serverKey := ed25519.NewKeyFromSeed(seed)
	var wall atomic.Int64
	wall.Store(fixture.Observation.Observation.SampleUnixMS)
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: fixture.Credentials.ClockTrust.RootPublicKey, Tenant: fixture.Credentials.Grant.Grant.Tenant, Site: fixture.Credentials.Grant.Grant.Site, Domain: "simulation", Audience: fixture.Credentials.Audience, Now: func() time.Time { return time.UnixMilli(wall.Load()) }, Replay: authority.NewMemoryReplay(100)})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifier.UpdateRevocations(fixture.Revocations); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/annotations/clock":
			if r.Header.Get("X-World-Authorization") != "" {
				t.Error("bootstrap required ordinary timestamp")
			}
			var q annotationclock.SignedRequest
			if authority.DecodeStrict(body, &q) != nil {
				t.Error("request")
				w.WriteHeader(400)
				return
			}
			o, e := verifier.IssueAnnotationClock(q, grant, serverKey, "office-run", "simulation:office-run", strings.Repeat("cd", 32))
			if e != nil {
				t.Error(e)
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(o)
		case "/v1/annotations/epoch":
			header, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
			var proof authority.SignedRequest
			_ = json.Unmarshal(header, &proof)
			if proof.IssuedAtUnixMS != wall.Load()-2 {
				t.Errorf("wrong derived request issuance %d vs %d", proof.IssuedAtUnixMS, wall.Load())
			}
			if _, e := verifier.Verify(proof, "annotation-write", body); e != nil {
				t.Error(e)
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte(`{"epoch":8}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	fixture.Credentials.URL = server.URL
	c, err := New(fixture.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ObserveAnnotationClock(context.Background(), nil); err == nil {
		t.Fatal("implicit unsafe clock")
	}
	var mono atomic.Int64
	calls := 0
	provider := func() time.Duration {
		calls++
		if calls == 2 {
			mono.Store(int64(10 * time.Millisecond))
		}
		return time.Duration(mono.Load())
	}
	clock, err := c.ObserveAnnotationClock(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := c.NextAnnotationEpochWithClock(context.Background(), clock)
	if err != nil || epoch != 8 {
		t.Fatal(epoch, err)
	}
	saved, err := clock.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	mono.Add(int64(time.Minute))
	later, err := clock.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if saved.ElapsedNS != 0 || later.ElapsedNS != int64(time.Minute) {
		t.Fatal("changed frozen evidence")
	}
	mono.Store(int64(20 * time.Millisecond))
	if _, err = clock.Evidence(); err == nil {
		t.Fatal("rollback after prior estimate")
	}
	mono.Store(int64(2 * time.Minute))
	if _, err = clock.Evidence(); err == nil {
		t.Fatal("regressed anchor recovered without a fresh observation")
	}
	mono.Store(int64(annotationclock.MaxHoldover + time.Second))
	if _, err = clock.Evidence(); err == nil {
		t.Fatal("expired holdover accepted")
	}
	if requests.Load() != 2 {
		t.Fatal("unexpected requests", requests.Load())
	}
}
