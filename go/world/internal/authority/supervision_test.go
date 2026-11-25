package authority

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

type supervisionFixture struct {
	*fixture
	gateway, operator       SignedGrant
	gatewayKey, operatorKey ed25519.PrivateKey
	c                       supervision.Challenge
}

func newSupervisionFixture(t *testing.T) *supervisionFixture {
	t.Helper()
	f := &supervisionFixture{fixture: newFixture(t, nil)}
	g := f.grant.Grant
	g.ID = "gateway-grant"
	g.Principal = "gateway-1"
	g.Kind = "device"
	g.Scopes = []string{"task-status", "supervision-server"}
	g.TaskStatusAssignee = "robot/app"
	g.TaskStatusAudience = "node-1"
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	g.PublicKey = pub
	f.gatewayKey = key
	var err error
	f.gateway, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	g.ID = "operator-grant"
	g.Principal = "operator-1"
	g.Kind = "operator"
	g.Scopes = []string{"supervise", "stop"}
	g.TaskStatusAssignee = ""
	g.TaskStatusAudience = ""
	pub, key, _ = ed25519.GenerateKey(rand.Reader)
	g.PublicKey = pub
	f.operatorKey = key
	f.operator, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.c = supervision.Challenge{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", RunID: "run-1", Audience: "node-1", Assignee: "robot/app", GatewayPrincipal: "gateway-1", GatewayBootID: "boot-1", TaskID: "task-1", SpecSHA256: strings.Repeat("a", 64), ExecutorEpoch: 1, OperatorPrincipal: "operator-1", OperatorKeySHA256: KeyID(pub), OperatorGrantID: g.ID, RequestID: "request_1234567890", ChallengeID: "challenge_1234567890", IssuedAtUnixMS: f.now.UnixMilli(), ExpiresAtUnixMS: f.now.Add(500 * time.Millisecond).UnixMilli()}
	return f
}
func (f *supervisionFixture) challenge(t *testing.T) SignedSupervisionChallenge {
	t.Helper()
	s, err := SignSupervisionChallenge(f.gateway, f.gatewayKey, f.c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *supervisionFixture) ack(t *testing.T) SignedSupervisionAck {
	t.Helper()
	s, err := SignSupervisionAck(f.operator, f.operatorKey, supervision.Ack{Challenge: f.c})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *supervisionFixture) receipt(t *testing.T, a SignedSupervisionAck) SignedSupervisionReceipt {
	t.Helper()
	s, err := SignSupervisionReceipt(f.gateway, f.gatewayKey, supervision.Receipt{Challenge: f.c, AckSHA256: SupervisionAckDigest(a)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestSupervisionHandshakeAndCanonicalProof(t *testing.T) {
	f := newSupervisionFixture(t)
	c := f.challenge(t)
	a := f.ack(t)
	r := f.receipt(t, a)
	if id, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err != nil || id.Principal != "gateway-1" {
		t.Fatal(id, err)
	}
	raw, _ := json.MarshalIndent(a, "", " ")
	for i := 0; i < 3; i++ {
		ack, op, err := f.verifier.VerifySupervisionAckProof(raw)
		if err != nil || ack.Challenge != f.c || op.Principal != "operator-1" || op.KeySHA256 != KeyID(f.operator.Grant.PublicKey) || op.GrantID != f.operator.Grant.ID || hash(op.Proof) != SupervisionAckDigest(a) {
			t.Fatalf("non-consuming proof %d: %+v %+v %v", i, ack, op, err)
		}
		raw = op.Proof
	}
	if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err != nil {
		t.Fatal(err)
	}
	if r.Receipt.Challenge.ExpiresAtUnixMS != f.c.ExpiresAtUnixMS {
		t.Fatal("receipt renewed expiry")
	}
	if _, err := f.verifier.VerifySupervisionChallenge(c, "other-gateway"); err == nil {
		t.Fatal("wrong gateway accepted")
	}
	if _, err := f.verifier.VerifySupervisionReceipt(r, "other-gateway"); err == nil {
		t.Fatal("wrong receipt gateway accepted")
	}
	f.now = f.now.Add(500 * time.Millisecond)
	if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
		t.Fatal("expired challenge accepted")
	}
	if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
		t.Fatal("expired ACK accepted")
	}
	if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err == nil {
		t.Fatal("expired receipt accepted")
	}
}
func TestSupervisionEveryBindingIsSigned(t *testing.T) {
	changes := map[string]func(*supervision.Challenge){
		"version": func(c *supervision.Challenge) { c.Version++ }, "tenant": func(c *supervision.Challenge) { c.Tenant = "other" }, "site": func(c *supervision.Challenge) { c.Site = "other" }, "domain": func(c *supervision.Challenge) { c.Domain = "physical" }, "run": func(c *supervision.Challenge) { c.RunID = "other" }, "audience": func(c *supervision.Challenge) { c.Audience = "other" }, "assignee": func(c *supervision.Challenge) { c.Assignee = "other" }, "gateway": func(c *supervision.Challenge) { c.GatewayPrincipal = "other" }, "boot": func(c *supervision.Challenge) { c.GatewayBootID = "other" }, "task": func(c *supervision.Challenge) { c.TaskID = "other" }, "spec": func(c *supervision.Challenge) { c.SpecSHA256 = strings.Repeat("b", 64) }, "epoch": func(c *supervision.Challenge) { c.ExecutorEpoch++ }, "operator": func(c *supervision.Challenge) { c.OperatorPrincipal = "other" }, "key": func(c *supervision.Challenge) { c.OperatorKeySHA256 = strings.Repeat("b", 64) }, "grant": func(c *supervision.Challenge) { c.OperatorGrantID = "other" }, "request": func(c *supervision.Challenge) { c.RequestID = "request_other123456" }, "challenge": func(c *supervision.Challenge) { c.ChallengeID = "challenge_other123456" }, "issued": func(c *supervision.Challenge) { c.IssuedAtUnixMS-- }, "expiry": func(c *supervision.Challenge) { c.ExpiresAtUnixMS++ },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newSupervisionFixture(t)
			c := f.challenge(t)
			a := f.ack(t)
			r := f.receipt(t, a)
			change(&c.Challenge)
			change(&a.Ack.Challenge)
			change(&r.Receipt.Challenge)
			if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
				t.Fatal("changed challenge accepted")
			}
			raw, _ := json.Marshal(a)
			if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
				t.Fatal("changed ACK accepted")
			}
			if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err == nil {
				t.Fatal("changed receipt accepted")
			}
		})
	}
}
func TestSupervisionGrantAndSignatureDomainBinding(t *testing.T) {
	f := newSupervisionFixture(t)
	c := f.challenge(t)
	a := f.ack(t)
	r := f.receipt(t, a)
	c.Grant.Grant.ID = "renewed-gateway"
	c.Grant, _ = SignGrant(c.Grant.Grant, f.root)
	if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
		t.Fatal("substituted valid gateway grant accepted")
	}
	// Signature bytes from one message class cannot authenticate another class.
	r.Signature = f.challenge(t).Signature
	if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err == nil {
		t.Fatal("challenge signature accepted as receipt")
	}
	a.Signature, _ = sign(f.operatorKey, "request", supervisionPayload(a.Version, a.Grant, a.Ack))
	raw, _ := json.Marshal(a)
	if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
		t.Fatal("wrong ACK signature domain accepted")
	}
	r = f.receipt(t, f.ack(t))
	r.Receipt.AckSHA256 = strings.Repeat("b", 64)
	if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err == nil {
		t.Fatal("altered ACK digest accepted")
	}
}
func TestSupervisionRevocationRecheckedWithoutNonce(t *testing.T) {
	for _, who := range []string{"operator", "gateway"} {
		for _, kind := range []string{"grant", "key", "principal"} {
			t.Run(who+"/"+kind, func(t *testing.T) {
				f := newSupervisionFixture(t)
				c := f.challenge(t)
				a := f.ack(t)
				r := f.receipt(t, a)
				raw, _ := json.Marshal(a)
				if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err != nil {
					t.Fatal(err)
				}
				g := f.operator.Grant
				if who == "gateway" {
					g = f.gateway.Grant
				}
				rev := f.rev.Revocations
				rev.Revision++
				switch kind {
				case "grant":
					rev.GrantIDs = []string{g.ID}
				case "key":
					rev.KeyIDs = []string{KeyID(g.PublicKey)}
				case "principal":
					rev.Principals = []string{g.Principal}
				}
				signed, _ := SignRevocations(rev, f.root)
				if err := f.verifier.UpdateRevocations(signed); err != nil {
					t.Fatal(err)
				}
				if who == "operator" {
					if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
						t.Fatal("revoked ACK accepted")
					}
				} else {
					if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
						t.Fatal("revoked gateway challenge accepted")
					}
					if _, err := f.verifier.VerifySupervisionReceipt(r, "gateway-1"); err == nil {
						t.Fatal("revoked gateway receipt accepted")
					}
				}
			})
		}
	}
}
func TestSupervisionRejectsScopesAndUnboundedGrantWindows(t *testing.T) {
	for name, change := range map[string]func(*Grant){
		"missing stop":        func(g *Grant) { g.Scopes = []string{"supervise"} },
		"app supervisor":      func(g *Grant) { g.Kind = "app" },
		"physical supervisor": func(g *Grant) { g.Domain = "physical" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSupervisionFixture(t)
			g := f.operator.Grant
			change(&g)
			if _, err := SignGrant(g, f.root); err == nil {
				t.Fatal("invalid supervisor grant issued")
			}
		})
	}
	for name, change := range map[string]func(*Grant){
		"missing status":   func(g *Grant) { g.Scopes = []string{"supervision-server"} },
		"missing assignee": func(g *Grant) { g.TaskStatusAssignee = "" },
		"missing audience": func(g *Grant) { g.TaskStatusAudience = "" },
		"operator server":  func(g *Grant) { g.Kind = "operator" },
		"physical server":  func(g *Grant) { g.Domain = "physical" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSupervisionFixture(t)
			g := f.gateway.Grant
			change(&g)
			if _, err := SignGrant(g, f.root); err == nil {
				t.Fatal("invalid server grant issued")
			}
		})
	}
	f := newSupervisionFixture(t)
	f.operator.Grant.NotAfterUnixMS = f.c.ExpiresAtUnixMS - 1
	f.operator, _ = SignGrant(f.operator.Grant, f.root)
	if _, err := SignSupervisionAck(f.operator, f.operatorKey, supervision.Ack{Challenge: f.c}); err == nil {
		t.Fatal("lease extends operator grant")
	}
	f.gateway.Grant.NotAfterUnixMS = f.c.ExpiresAtUnixMS - 1
	f.gateway, _ = SignGrant(f.gateway.Grant, f.root)
	if _, err := SignSupervisionChallenge(f.gateway, f.gatewayKey, f.c); err == nil {
		t.Fatal("lease extends gateway grant")
	}
}
func TestSupervisionFutureClockStaleRevocationAndStrictDecode(t *testing.T) {
	f := newSupervisionFixture(t)
	c := f.challenge(t)
	a := f.ack(t)
	raw, _ := json.Marshal(a)
	f.now = f.now.Add(-time.Millisecond)
	if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
		t.Fatal("future challenge accepted")
	}
	if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
		t.Fatal("future ACK accepted")
	}
	f.now = f.now.Add(time.Millisecond)
	rev := f.rev.Revocations
	rev.Revision++
	rev.ExpiresAtUnixMS = f.now.Add(100 * time.Millisecond).UnixMilli()
	signed, _ := SignRevocations(rev, f.root)
	if err := f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(100 * time.Millisecond)
	if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
		t.Fatal("stale revocation accepted")
	}
	for _, bad := range []string{`{"version":1,"version":1}`, `{"version":1,"unknown":1}`, string(raw) + ` {}`} {
		if _, _, err := f.verifier.VerifySupervisionAckProof([]byte(bad)); err == nil {
			t.Fatal("malformed proof accepted")
		}
	}
}

func TestSupervisionWireMaximumLease(t *testing.T) {
	f := newSupervisionFixture(t)
	f.c.ExpiresAtUnixMS = f.c.IssuedAtUnixMS + supervision.MaxLease.Milliseconds() + 1
	if _, err := SignSupervisionChallenge(f.gateway, f.gatewayKey, f.c); err == nil {
		t.Fatal("over-limit challenge signed")
	}
	if _, err := SignSupervisionAck(f.operator, f.operatorKey, supervision.Ack{Challenge: f.c}); err == nil {
		t.Fatal("over-limit ACK signed")
	}
	r := supervision.Receipt{Challenge: f.c, AckSHA256: strings.Repeat("a", 64)}
	if _, err := SignSupervisionReceipt(f.gateway, f.gatewayKey, r); err == nil {
		t.Fatal("over-limit receipt signed")
	}
	// A hostile holder may bypass signing helpers. Verifiers independently enforce
	// the wire bound even with otherwise valid keys, grants and signatures.
	c := SignedSupervisionChallenge{Version: 1, Grant: f.gateway, Challenge: f.c}
	c.Signature, _ = sign(f.gatewayKey, "supervision-challenge", supervisionPayload(c.Version, c.Grant, c.Challenge))
	if _, err := f.verifier.VerifySupervisionChallenge(c, "gateway-1"); err == nil {
		t.Fatal("over-limit signed challenge accepted")
	}
	a := SignedSupervisionAck{Version: 1, Grant: f.operator, Ack: supervision.Ack{Challenge: f.c}}
	a.Signature, _ = sign(f.operatorKey, "supervision-ack", supervisionPayload(a.Version, a.Grant, a.Ack))
	raw, _ := json.Marshal(a)
	if _, _, err := f.verifier.VerifySupervisionAckProof(raw); err == nil {
		t.Fatal("over-limit signed ACK accepted")
	}
	receipt := SignedSupervisionReceipt{Version: 1, Grant: f.gateway, Receipt: r}
	receipt.Signature, _ = sign(f.gatewayKey, "supervision-receipt", supervisionPayload(receipt.Version, receipt.Grant, receipt.Receipt))
	if _, err := f.verifier.VerifySupervisionReceipt(receipt, "gateway-1"); err == nil {
		t.Fatal("over-limit signed receipt accepted")
	}
}
