package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type supervisionFixture struct {
	credentials        Config
	trust              SupervisionTrust
	target             SupervisionTarget
	serverGrant        authority.SignedGrant
	serverKey          ed25519.PrivateKey
	verifier           *authority.Verifier
	mu                 sync.Mutex
	calls, acks        int
	requestIDs         []string
	change             func(*supervision.Challenge, int)
	receiptChange      func(*supervision.Receipt)
	corrupt, duplicate bool
	block              bool
}

func newSupervisionFixture(t *testing.T) *supervisionFixture {
	t.Helper()
	now := time.Now()
	root, rootKey, _ := ed25519.GenerateKey(rand.Reader)
	opPub, opKey, _ := ed25519.GenerateKey(rand.Reader)
	serverPub, serverKey, _ := ed25519.GenerateKey(rand.Reader)
	grant := func(name, kind string, key ed25519.PublicKey, scopes []string) authority.Grant {
		return authority.Grant{Version: 1, ID: name + "-grant", Tenant: "lab", Site: "office", Domain: "simulation", Principal: name, Kind: kind, Source: world.Source{Device: name, App: "world", Service: name, Stream: "supervision"}, PublicKey: key, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}
	}
	op, err := authority.SignGrant(grant("operator", "operator", opPub, []string{"supervise", "stop", "read"}), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	sg := grant("gateway", "device", serverPub, []string{"supervision-server", "task-status"})
	sg.TaskStatusAssignee = "robot/nav"
	sg.TaskStatusAudience = "node"
	serverGrant, err := authority.SignGrant(sg, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "lab", Site: "office", Domain: "simulation", Audience: "node", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	spec := tasking.Spec{ID: "mission", Site: "office", Type: "wendy.nav.go_to.v1", Assignee: "robot/nav", Domain: mission.Simulation, Mode: mission.Supervised, Class: mission.Move, Frame: world.FrameRef{ID: "floor", Revision: "1"}, NotAfterUnixMS: now.Add(time.Minute).UnixMilli()}
	return &supervisionFixture{credentials: Config{Audience: "node", Grant: op, PrivateKey: opKey, AllowInsecureSimulation: true}, trust: SupervisionTrust{RootPublicKey: root, Revocations: rev}, target: SupervisionTarget{Spec: spec, RunID: "run", GatewayPrincipal: "gateway", Now: time.Now}, serverGrant: serverGrant, serverKey: serverKey, verifier: v}
}
func (f *supervisionFixture) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		calls := f.calls
		f.mu.Unlock()
		raw, err := io.ReadAll(io.LimitReader(r.Body, authority.MaxEnvelopeBytes+1))
		if err != nil || len(raw) > authority.MaxEnvelopeBytes {
			http.Error(w, "bad", 400)
			return
		}
		if f.block {
			<-r.Context().Done()
			return
		}
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err != nil {
			t.Error(err)
			http.Error(w, "bad", 400)
			return
		}
		request, err := authority.DecodeRequest(header)
		if err != nil {
			t.Error(err)
			http.Error(w, "bad", 400)
			return
		}
		identity, err := f.verifier.Verify(request, "supervise", raw)
		if err != nil {
			t.Error(err)
			http.Error(w, "denied", 401)
			return
		}
		if r.Method != "POST" {
			t.Error("wrong method")
			return
		}
		switch r.URL.Path {
		case "/v1/tasks/supervision/challenge":
			var body struct {
				TaskID     string `json:"task_id"`
				TaskDigest string `json:"task_digest"`
				RequestID  string `json:"request_id"`
			}
			if authority.DecodeStrict(raw, &body) != nil {
				t.Error("invalid challenge body")
				return
			}
			f.mu.Lock()
			f.requestIDs = append(f.requestIDs, body.RequestID)
			f.mu.Unlock()
			now := time.Now()
			ch := supervision.Challenge{Version: 1, Tenant: "lab", Site: "office", Domain: "simulation", RunID: "run", Audience: "node", Assignee: "robot/nav", GatewayPrincipal: "gateway", GatewayBootID: "boot", TaskID: body.TaskID, SpecSHA256: body.TaskDigest, ExecutorEpoch: 1, OperatorPrincipal: identity.Principal, OperatorKeySHA256: identity.SigningKeySHA256, OperatorGrantID: identity.GrantID, RequestID: body.RequestID, ChallengeID: fmt.Sprintf("challenge-%016d", calls), IssuedAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(400 * time.Millisecond).UnixMilli()}
			if f.change != nil {
				f.change(&ch, calls)
			}
			signed, err := authority.SignSupervisionChallenge(f.serverGrant, f.serverKey, ch)
			if err != nil {
				t.Error(err)
				return
			}
			if f.corrupt {
				signed.Signature[0] ^= 1
			}
			data, _ := json.Marshal(signed)
			if f.duplicate {
				data = append([]byte(`{"version":1,`), data[1:]...)
			}
			_, _ = w.Write(data)
		case "/v1/tasks/supervision/ack":
			f.mu.Lock()
			f.acks++
			f.mu.Unlock()
			ack, op, err := f.verifier.VerifySupervisionAckProof(raw)
			if err != nil {
				t.Error(err)
				http.Error(w, "bad ack", 401)
				return
			}
			if op.Principal != identity.Principal {
				t.Error("outer/inner operator mismatch")
			}
			var signedAck authority.SignedSupervisionAck
			if authority.DecodeStrict(raw, &signedAck) != nil {
				t.Error("invalid signed ack")
				return
			}
			receipt := supervision.Receipt{Challenge: ack.Challenge, AckSHA256: authority.SupervisionAckDigest(signedAck)}
			if f.receiptChange != nil {
				f.receiptChange(&receipt)
			}
			signed, err := authority.SignSupervisionReceipt(f.serverGrant, f.serverKey, receipt)
			if err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(signed)
		default:
			t.Error("unexpected endpoint")
		}
	})
}
func (f *supervisionFixture) start(t *testing.T) *Supervisor {
	t.Helper()
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	f.credentials.URL = server.URL
	s, err := NewSupervisor(f.credentials, f.trust, f.target)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestSupervisionRoundTripFreshRequestsAndBoundedReceipt(t *testing.T) {
	f := newSupervisionFixture(t)
	s := f.start(t)
	var last SupervisionLease
	for i := 0; i < 2; i++ {
		lease, err := s.Renew(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if lease.Domain != "simulation" || lease.TaskID != "mission" || lease.ExpiresAtUnixMS-lease.IssuedAtUnixMS != 400 || lease.AckSHA256 == "" {
			t.Fatal("acknowledged lease lost binding or gained time")
		}
		last = lease
	}
	if f.acks != 2 || len(f.requestIDs) != 2 || f.requestIDs[0] == f.requestIDs[1] || len(f.requestIDs[0]) != 32 {
		t.Fatal("request IDs not fresh or full exchanges missing")
	}
	wire, _ := json.Marshal(last)
	if strings.Contains(string(wire), base64.StdEncoding.EncodeToString(f.credentials.PrivateKey)) {
		t.Fatal("private key leaked")
	}
}
func TestSupervisionRejectsChallengeBindingsBeforeAck(t *testing.T) {
	changes := map[string]func(*supervision.Challenge){"run": func(c *supervision.Challenge) { c.RunID = "other" }, "task": func(c *supervision.Challenge) { c.TaskID = "other" }, "digest": func(c *supervision.Challenge) { c.SpecSHA256 = strings.Repeat("0", 64) }, "operator": func(c *supervision.Challenge) { c.OperatorPrincipal = "other" }, "key": func(c *supervision.Challenge) { c.OperatorKeySHA256 = strings.Repeat("0", 64) }, "grant": func(c *supervision.Challenge) { c.OperatorGrantID = "other" }, "request": func(c *supervision.Challenge) { c.RequestID = "other-request-identifier" }, "expired": func(c *supervision.Challenge) { c.IssuedAtUnixMS -= 1000; c.ExpiresAtUnixMS -= 1000 }, "future": func(c *supervision.Challenge) { c.IssuedAtUnixMS += 1000; c.ExpiresAtUnixMS += 1000 }}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newSupervisionFixture(t)
			f.change = func(c *supervision.Challenge, _ int) { change(c) }
			s := f.start(t)
			if _, err := s.Renew(context.Background()); err == nil {
				t.Fatal("mismatched challenge accepted")
			}
			if f.acks != 0 {
				t.Fatal("untrusted challenge acknowledged")
			}
			calls := f.calls
			if _, err := s.Renew(context.Background()); err == nil || f.calls != calls {
				t.Fatal("failed exchange retried implicitly")
			}
		})
	}
	for _, kind := range []string{"signature", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := newSupervisionFixture(t)
			f.corrupt = kind == "signature"
			f.duplicate = kind == "duplicate"
			s := f.start(t)
			if _, err := s.Renew(context.Background()); err == nil || f.acks != 0 {
				t.Fatal("invalid envelope reached ack")
			}
		})
	}
}
func TestSupervisionRejectsReceiptMismatch(t *testing.T) {
	for _, kind := range []string{"ack digest", "challenge"} {
		t.Run(kind, func(t *testing.T) {
			f := newSupervisionFixture(t)
			f.receiptChange = func(r *supervision.Receipt) {
				if kind == "ack digest" {
					r.AckSHA256 = strings.Repeat("0", 64)
				} else {
					r.Challenge.ChallengeID = "another-challenge-identifier"
				}
			}
			s := f.start(t)
			if _, err := s.Renew(context.Background()); err == nil {
				t.Fatal("mismatched receipt accepted")
			}
			if f.acks != 1 {
				t.Fatal("test never reached ack")
			}
		})
	}
}
func TestSupervisionGenerationChangeStopsBeforeAck(t *testing.T) {
	for _, kind := range []string{"boot", "epoch"} {
		t.Run(kind, func(t *testing.T) {
			f := newSupervisionFixture(t)
			f.change = func(c *supervision.Challenge, calls int) {
				if calls > 2 {
					if kind == "boot" {
						c.GatewayBootID = "newboot"
					} else {
						c.ExecutorEpoch++
					}
				}
			}
			s := f.start(t)
			if _, err := s.Renew(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Renew(context.Background()); err == nil || f.acks != 1 {
				t.Fatal("new generation silently accepted")
			}
		})
	}
}
func TestSupervisionTimeoutAndRedirectAreTerminal(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newSupervisionFixture(t)
		f.block = true
		s := f.start(t)
		start := time.Now()
		if _, err := s.Renew(context.Background()); err == nil || err.Error() != "supervision challenge exchange failed (timeout)" {
			t.Fatalf("blocked exchange classification: %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("round exceeded bound")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		f := newSupervisionFixture(t)
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("credential sent to redirect") }))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
		defer server.Close()
		f.credentials.URL = server.URL
		s, err := NewSupervisor(f.credentials, f.trust, f.target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Renew(context.Background()); err == nil {
			t.Fatal("redirect accepted")
		}
	})
}

func TestSupervisionHTTPFailureReportsOnlyStageAndStatus(t *testing.T) {
	for _, stage := range []string{"challenge", "acknowledgement"} {
		for _, status := range []int{401, 403, 409, 503} {
			t.Run(fmt.Sprintf("%s-%d", stage, status), func(t *testing.T) {
				f := newSupervisionFixture(t)
				good := f.handler(t)
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if (stage == "challenge" && strings.HasSuffix(r.URL.Path, "/challenge")) || (stage == "acknowledgement" && strings.HasSuffix(r.URL.Path, "/ack")) {
						http.Error(w, "private-response-marker "+r.Header.Get("X-World-Authorization"), status)
						return
					}
					good.ServeHTTP(w, r)
				}))
				defer server.Close()
				f.credentials.URL = server.URL
				s, err := NewSupervisor(f.credentials, f.trust, f.target)
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.Renew(context.Background())
				want := fmt.Sprintf("supervision %s exchange failed (HTTP %d)", stage, status)
				if err == nil || err.Error() != want {
					t.Fatalf("unsafe or missing classification: %v", err)
				}
				before := calls
				if _, err = s.Renew(context.Background()); err == nil || calls != before {
					t.Fatal("classified failure retried")
				}
			})
		}
	}
}

func TestSupervisionExchangeClassificationDoesNotWrapPrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		name, class string
		cause       error
	}{
		{"cancel", "cancelled", fmt.Errorf("private-cause: %w", context.Canceled)},
		{"deadline", "timeout", &url.Error{Op: "POST", URL: "https://private.invalid/proof", Err: context.DeadlineExceeded}},
		{"network timeout", "timeout", &net.DNSError{Err: "private-cause", Name: "private.invalid", IsTimeout: true}},
		{"transport", "transport or response", errors.New("private-cause")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := supervisionExchangeError("challenge", context.Background(), tc.cause)
			if err.Error() != "supervision challenge exchange failed ("+tc.class+")" || errors.Unwrap(err) != nil {
				t.Fatal("classification retained private cause")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := supervisionExchangeError("acknowledgement", ctx, io.EOF).Error(); got != "supervision acknowledgement exchange failed (cancelled)" {
		t.Fatal(got)
	}
}

func TestHTTPStatusErrorPreservesClientDiagnostic(t *testing.T) {
	f := newSupervisionFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "existing diagnostic", 409)
	}))
	defer server.Close()
	f.credentials.URL = server.URL
	c, err := New(f.credentials)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), "supervise", "POST", "/v1/tasks/supervision/challenge", []byte("{}"), nil)
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.StatusCode != 409 || err.Error() != "node HTTP 409: existing diagnostic" {
		t.Fatal("existing client diagnostic or typed status changed")
	}
}
func TestSupervisionRequiresExplicitSimulationTrustAndMode(t *testing.T) {
	for _, kind := range []string{"autonomous", "physical", "run", "gateway", "clock", "root", "revocations"} {
		t.Run(kind, func(t *testing.T) {
			f := newSupervisionFixture(t)
			f.credentials.URL = "http://127.0.0.1:1"
			switch kind {
			case "autonomous":
				f.target.Spec.Mode = mission.Autonomous
			case "physical":
				f.target.Spec.Domain = mission.Physical
			case "run":
				f.target.RunID = ""
			case "gateway":
				f.target.GatewayPrincipal = ""
			case "clock":
				f.target.Now = nil
			case "root":
				f.trust.RootPublicKey = make([]byte, 32)
			case "revocations":
				f.trust.Revocations.Signature = nil
			}
			if _, err := NewSupervisor(f.credentials, f.trust, f.target); err == nil {
				t.Fatal("unsafe supervisor configuration accepted")
			}
		})
	}
}
