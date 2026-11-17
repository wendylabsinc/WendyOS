package revocation

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

func TestReissuedNumberAdvancesEvidenceAndPersistsFloor(t *testing.T) {
	for _, pq := range []bool{false, true} {
		for _, source := range []string{"DER fetch", "PEM fetch", "staple"} {
			t.Run(fmt.Sprintf("pq=%v/%s", pq, source), func(t *testing.T) {
				p := crltest.New(t, pq)
				leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/operator/alice")
				path := []*x509.Certificate{leaf.Cert, p.CA}
				now := time.Now().UTC().Truncate(time.Second)
				sign := func(number int64, at time.Time, revoked bool) []byte {
					crl := &x509.RevocationList{Number: big.NewInt(number), ThisUpdate: at, NextUpdate: at.Add(time.Hour)}
					if revoked {
						crl.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: at}}
					}
					return p.Sign(t, crl)
				}
				old := sign(1, now.Add(-2*time.Hour), false)
				next := sign(1, now.Add(-time.Minute), true)
				dir := t.TempDir()
				m := configured(t, p.Client, Config{Dir: dir})
				if err := m.adopt(p.CA, old, true); err != nil {
					t.Fatal(err)
				}
				revision := m.Revision()
				if source == "staple" {
					m.Offer(next)
				} else {
					if source == "PEM fetch" {
						next = pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: next})
					}
					p.Respond(next, 200)
					m.register(leaf.Cert, p.CA, issuerCacheKey(p.CA))
					m.Refresh(context.Background())
				}
				if err := m.CheckPath(path, now, Session); !errors.Is(err, ErrRevoked) {
					t.Fatalf("renewed CRL ignored: %v", err)
				}
				if m.Revision() == revision {
					t.Fatal("renewal did not invalidate caches")
				}
				other := p.Issue(t, 43, "spiffe://wendy.sh/tenant/t/operator/bob")
				if err := m.Check(other.Cert, p.CA, now, Session); err != nil {
					t.Fatalf("fresh renewal did not admit unrevoked operator: %v", err)
				}
				restarted := configured(t, offline(), Config{Dir: dir})
				if err := restarted.CheckPath(path, now, Session); !errors.Is(err, ErrRevoked) {
					t.Fatalf("restart lost renewal: %v", err)
				}
				for _, raw := range [][]byte{old, sign(0, now, true), sign(2, now.Add(-time.Hour), false), sign(1, now.Add(-time.Minute), false)} {
					if err := restarted.adopt(p.CA, raw, true); err == nil {
						t.Error("accepted rollback or equal-time conflict")
					}
					restarted.Offer(raw)
					if err := restarted.CheckPath(path, now, RevokedOnly); !errors.Is(err, ErrRevoked) {
						t.Fatalf("rollback restored revoked certificate: %v", err)
					}
				}
			})
		}
	}
}

func TestRejectedDownloadsBackOffAndLog(t *testing.T) {
	for _, reason := range []string{"HTTP", "malformed", "unsupported", "rollback"} {
		t.Run(reason, func(t *testing.T) {
			p := crltest.New(t, false)
			leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/operator/alice")
			var logs []string
			m := configured(t, p.Client, Config{Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }})
			if err := m.Check(leaf.Cert, p.CA, time.Now(), Session); err != nil {
				t.Fatal(err)
			}
			e := m.register(leaf.Cert, p.CA, issuerCacheKey(p.CA))[0]
			switch reason {
			case "HTTP":
				p.Respond(nil, 503)
			case "malformed":
				p.Respond(make([]byte, 99<<10), 200)
			case "unsupported":
				p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour), ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 28}, Critical: true, Value: []byte{0x30, 0}}}}), 200)
			case "rollback":
				p.Publish(t, 0)
			}
			for attempt := 0; attempt < 12; attempt++ {
				e.mu.Lock()
				e.nextAttempt = time.Time{}
				e.mu.Unlock()
				start := time.Now()
				m.refresh(context.Background(), e, false)
				want := min(retryInterval<<min(attempt, 7), maxRetryInterval)
				e.mu.Lock()
				delay := e.nextAttempt.Sub(start)
				e.mu.Unlock()
				if delay < want || delay > want+time.Second {
					t.Fatalf("attempt %d retry delay %s, want %s", attempt, delay, want)
				}
				hits := p.Hits.Load()
				// Both background work and admission attempts must respect the deadline.
				for range 20 {
					m.refreshEntries(context.Background(), false)
					_ = m.Check(leaf.Cert, p.CA, time.Now().Add(2*time.Hour), Session)
				}
				if p.Hits.Load() != hits {
					t.Fatal("checks bypassed backoff and downloaded repeatedly")
				}
			}
			if len(logs) != 1 || !strings.Contains(logs[0], "CRL refresh failed") || !strings.Contains(logs[0], crltest.Endpoint) {
				t.Fatalf("missing/rate-unlimited diagnostic: %v", logs)
			}
			p.Publish(t, 3)
			m.Refresh(context.Background())
			if e.failures != 0 {
				t.Fatal("success did not reset retry backoff")
			}
		})
	}
}

func TestPEMCRLValidation(t *testing.T) {
	p := crltest.New(t, false)
	raw := p.Sign(t, &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour)})
	valid := pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: raw})
	for name, data := range map[string][]byte{"wrong label": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), "trailing": append(append([]byte(nil), valid...), valid...), "malformed": []byte("-----BEGIN X509 CRL-----\nwrong"), "oversized": make([]byte, maxCRLBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if New(nil).adopt(p.CA, data, true) == nil {
				t.Fatal("invalid encoding accepted")
			}
		})
	}
	raw[len(raw)-1] ^= 1
	if New(nil).adopt(p.CA, pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: raw}), true) == nil {
		t.Fatal("PEM bypassed signature verification")
	}
}
