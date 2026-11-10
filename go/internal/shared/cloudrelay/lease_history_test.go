package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/protobuf/proto"
)

func leaseHistoryFixture(t *testing.T) (*Agent, *testRelay, claims) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	a := &Agent{Verifier: v, StateDir: t.TempDir()}
	if err := a.keys(); err != nil {
		t.Fatal(err)
	}
	r := &testRelay{t: t, cloudKey: key, verifier: v}
	now := time.Now().Unix()
	c := claims{Iss: v.Issuer, Aud: "history-broker", Iat: now, Exp: now + 300, JTI: "current", Correlation: "history-test", Route: "history-route", Signing: binding(publicDER(a.signing)), Encryption: binding(publicDER(a.encryption))}
	return a, r, c
}

func historyLease(r *testRelay, c claims) *pb.IssuePresenceLeaseResponse {
	return &pb.IssuePresenceLeaseResponse{PresenceLeaseJws: r.artifact(c, leaseType), Broker: &pb.BrokerInstance{Endpoint: "broker.test:443", Audience: c.Aud}}
}

func TestPresenceLeaseHistoryRestoresOnlyVerifiedCurrentIdentityAndRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*claims)
		damage func(string) error
		keep   bool
	}{
		{name: "valid", keep: true},
		{name: "expired", edit: func(c *claims) { c.Iat -= 400; c.Exp -= 400 }},
		{name: "other signing key", edit: func(c *claims) { c.Signing = binding([]byte("other agent")) }},
		{name: "other encryption key", edit: func(c *claims) { c.Encryption = binding([]byte("other key")) }},
		{name: "other issuer", edit: func(c *claims) { c.Iss = "https://other-cloud.test" }},
		{name: "other route", edit: func(c *claims) { c.Route = "other-route" }},
		{name: "other audience", edit: func(c *claims) { c.Aud = "other-broker" }},
		{name: "invalid signature", damage: func(path string) error {
			lease, err := readPresenceLease(path)
			if err != nil {
				return err
			}
			parts := strings.Split(lease.PresenceLeaseJws, ".")
			sig, err := decode64(parts[2])
			if err != nil {
				return err
			}
			sig[0] ^= 1
			parts[2] = b64.EncodeToString(sig)
			lease.PresenceLeaseJws = strings.Join(parts, ".")
			raw, err := proto.Marshal(lease)
			if err != nil {
				return err
			}
			return os.WriteFile(path, raw, 0600)
		}},
		{name: "malformed", damage: func(path string) error { return os.WriteFile(path, []byte("malformed"), 0600) }},
		{name: "oversized", damage: func(path string) error { return os.WriteFile(path, make([]byte, presenceLeaseBytesLimit+1), 0600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r, current := leaseHistoryFixture(t)
			prior := current
			prior.JTI = "prior"
			if tc.edit != nil {
				tc.edit(&prior)
			}
			if err := a.archivePresenceLease(historyLease(r, prior), prior); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(prior.JTI))
			if tc.damage != nil {
				if err := tc.damage(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.restorePresenceLeases(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			_, got := a.leases[prior.JTI]
			if got != tc.keep || a.leases[current.JTI] != current {
				t.Fatalf("prior restored=%t, want %t; current lease=%+v", got, tc.keep, a.leases[current.JTI])
			}
			if tc.name == "expired" || tc.damage != nil {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid or expired cache file was not removed: %v", err)
				}
			}
		})
	}
}

func TestPresenceLeaseHistoryIsPrivateBoundedAndKeepsCurrentLease(t *testing.T) {
	a, r, current := leaseHistoryFixture(t)
	for i := 0; i < presenceLeaseHistoryLimit+2; i++ {
		c := current
		c.JTI = fmt.Sprint(i)
		a.remember(c)
		if err := a.archivePresenceLease(historyLease(r, c), c); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.leases) != presenceLeaseHistoryLimit {
		t.Fatalf("unbounded memory history: %d", len(a.leases))
	}
	if err := a.archivePresenceLease(historyLease(r, current), current); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(current.JTI))
	// The active lease must survive even if the local clock moves backwards.
	if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	// Simulate one extra complete record left by an older process. Restoration
	// must enforce its bound without evicting the active, back-dated file.
	overflow := current
	overflow.JTI = "overflow"
	raw, err := proto.Marshal(historyLease(r, overflow))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeState(filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(overflow.JTI)), raw); err != nil {
		t.Fatal(err)
	}
	a.leases = nil
	if err := a.restorePresenceLeases(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(a.presenceLeaseHistoryDir())
	if err != nil || len(entries) != presenceLeaseHistoryLimit || len(a.leases) != presenceLeaseHistoryLimit || a.leases[current.JTI] != current {
		t.Fatalf("bad bounded history: files=%d memory=%d error=%v", len(entries), len(a.leases), err)
	}
	info, err := os.Stat(a.presenceLeaseHistoryDir())
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("history directory is not private: %v %v", info, err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0600 || info.Size() > presenceLeaseBytesLimit {
			t.Fatalf("invalid cache file mode or size: %v %v", info, err)
		}
	}
	tooLarge := historyLease(r, current)
	tooLarge.PresenceLeaseJws = strings.Repeat("x", presenceLeaseBytesLimit+1)
	if err := a.archivePresenceLease(tooLarge, current); err == nil {
		t.Fatal("oversized lease archived")
	}
}

type historyKeyTransport func(*http.Request) (*http.Response, error)

func (f historyKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPresenceLeaseHistoryRetainsUnverifiedRecordsDuringKeyOutage(t *testing.T) {
	a, r, current := leaseHistoryFixture(t)
	prior := current
	prior.JTI = "prior-during-key-outage"
	if err := a.archivePresenceLease(historyLease(r, prior), prior); err != nil {
		t.Fatal(err)
	}
	a.remember(current)
	publicKeys, err := json.Marshal(struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{a.Verifier.keys["test"]}})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := true
	a.Verifier.fetched = time.Now().Add(-6 * time.Minute)
	a.Verifier.HTTP = &http.Client{Transport: historyKeyTransport(func(*http.Request) (*http.Response, error) {
		code, body := http.StatusOK, string(publicKeys)
		if unavailable {
			code, body = http.StatusServiceUnavailable, "temporarily unavailable"
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	if err := a.restorePresenceLeases(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(prior.JTI))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unavailable keys caused valid signed history to be discarded: %v", err)
	}
	if _, ok := a.leases[prior.JTI]; ok || a.leases[current.JTI] != current {
		t.Fatal("unverified history was admitted, or the current verified lease was lost")
	}
	unavailable = false
	if err := a.restorePresenceLeases(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if a.leases[prior.JTI] != prior {
		t.Fatal("preserved history was not verified and restored once keys recovered")
	}
}
