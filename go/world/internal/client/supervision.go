package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const SupervisionRefresh = 100 * time.Millisecond
const SupervisionRoundTimeout = 250 * time.Millisecond
const SupervisionMaxLease = supervision.MaxLease

type SupervisionTrust struct {
	RootPublicKey []byte                      `json:"root_public_key"`
	Revocations   authority.SignedRevocations `json:"revocations"`
}
type SupervisionTarget struct {
	Spec                    tasking.Spec
	RunID, GatewayPrincipal string
	// The current lab adapter uses the explicitly supplied shared-host wall clock.
	// It is not the production authenticated-time provider or simulation elapsed time.
	Now func() time.Time
	// Metrics is optional, local timing only; it never records proof material.
	Metrics *runtimeobs.Collector
}
type SupervisionLease struct {
	Schema            string `json:"schema"`
	Domain            string `json:"domain"`
	TaskID            string `json:"task_id"`
	SpecSHA256        string `json:"spec_sha256"`
	RunID             string `json:"run_id"`
	OperatorPrincipal string `json:"operator_principal"`
	GatewayPrincipal  string `json:"gateway_principal"`
	GatewayBootID     string `json:"gateway_boot_id"`
	ExecutorEpoch     uint64 `json:"executor_epoch"`
	RequestID         string `json:"request_id"`
	ChallengeID       string `json:"challenge_id"`
	IssuedAtUnixMS    int64  `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS   int64  `json:"expires_at_unix_ms"`
	AckSHA256         string `json:"ack_sha256"`
}

// Supervisor has only the two supervision endpoints. A failed exchange latches
// the instance closed: retries require explicit caller action with a new instance.
// An acknowledged lease is not a motion decision or physical stop confirmation.
type Supervisor struct {
	mu       sync.Mutex
	client   *Client
	verifier *authority.Verifier
	target   SupervisionTarget
	digest   string
	boot     string
	epoch    uint64
	failed   bool
}

func NewSupervisor(credentials Config, trust SupervisionTrust, target SupervisionTarget) (*Supervisor, error) {
	g := credentials.Grant.Grant
	u, parseErr := url.Parse(credentials.URL)
	if parseErr != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("explicit supervision endpoint origin required")
	}
	if target.Now == nil || target.RunID == "" || len(target.RunID) > 256 || target.GatewayPrincipal == "" || len(target.GatewayPrincipal) > 256 || target.Spec.ID == "" || len(target.Spec.ID) > 256 || target.Spec.Domain != mission.Simulation || target.Spec.Mode != mission.Supervised || target.Spec.Assignee == "" || target.Spec.Frame.Validate() != nil || target.Spec.Site != g.Site || g.Domain != "simulation" || g.Kind != "operator" {
		return nil, errors.New("explicit supervised simulation task, operator, target and clock required")
	}
	if target.Spec.NotAfterUnixMS <= target.Now().UnixMilli() {
		return nil, errors.New("supervised task deadline has passed")
	}
	if _, err := json.Marshal(target.Spec); err != nil {
		return nil, errors.New("invalid supervised task specification")
	}
	// Freeze caller-owned key/grant slices before any request or signature.
	encoded, err := json.Marshal(credentials)
	if err != nil || len(encoded) > authority.MaxEnvelopeBytes {
		return nil, errors.New("bounded simulation credentials required")
	}
	defer clear(encoded)
	if err = authority.DecodeStrict(encoded, &credentials); err != nil {
		return nil, errors.New("invalid simulation credentials")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(trust.RootPublicKey), Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: credentials.Audience, Now: target.Now, Replay: authority.NewMemoryReplay(16), MaxRequestLifetime: SupervisionMaxLease})
	if err != nil {
		return nil, errors.New("invalid supervision trust")
	}
	if err = v.UpdateRevocations(trust.Revocations); err != nil {
		return nil, errors.New("supervision revocations unavailable or invalid")
	}
	id, err := v.VerifyGrant(credentials.Grant, "supervise")
	if err != nil || !id.Allows("stop") {
		return nil, errors.New("current operator supervise and stop authority required")
	}
	c, err := New(credentials)
	if err != nil {
		return nil, errors.New("invalid supervision endpoint or credentials")
	}
	return &Supervisor{client: c, verifier: v, target: target, digest: tasking.SpecDigest(target.Spec)}, nil
}
func (s *Supervisor) Renew(ctx context.Context) (lease SupervisionLease, err error) {
	if s != nil && s.target.Metrics != nil {
		started := time.Now()
		defer func() {
			s.target.Metrics.Observe(runtimeobs.SupervisorExchange, time.Since(started))
			if err != nil {
				s.target.Metrics.Increment(runtimeobs.SupervisorExchangeFailures)
			}
		}()
	}
	if s == nil || !s.mu.TryLock() {
		return lease, errors.New("supervisor unavailable or exchange already active")
	}
	defer s.mu.Unlock()
	if s.failed {
		return lease, errors.New("supervision exchange previously failed; no automatic retry")
	}
	defer func() {
		if err != nil {
			s.failed = true
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		return lease, errors.New("supervision cancelled")
	}
	ctx, cancel := context.WithTimeout(ctx, SupervisionRoundTimeout)
	defer cancel()
	if s.target.Now().UnixMilli() >= s.target.Spec.NotAfterUnixMS {
		return lease, errors.New("supervised task expired")
	}
	id, e := s.verifier.VerifyGrant(s.client.config.Grant, "supervise")
	if e != nil || !id.Allows("stop") {
		return lease, errors.New("supervisor authority no longer valid")
	}
	nonce := make([]byte, 24)
	if _, e = rand.Read(nonce); e != nil {
		return lease, errors.New("supervision request randomness unavailable")
	}
	requestID := base64.RawURLEncoding.EncodeToString(nonce)
	requestBody, _ := json.Marshal(struct {
		TaskID     string `json:"task_id"`
		TaskDigest string `json:"task_digest"`
		RequestID  string `json:"request_id"`
	}{s.target.Spec.ID, s.digest, requestID})
	var wire json.RawMessage
	if e = s.client.Do(ctx, "supervise", "POST", "/v1/tasks/supervision/challenge", requestBody, &wire); e != nil {
		return lease, supervisionExchangeError("challenge", ctx, e)
	}
	var signed authority.SignedSupervisionChallenge
	if len(wire) > authority.MaxEnvelopeBytes || authority.DecodeStrict(wire, &signed) != nil {
		return lease, errors.New("invalid supervision challenge response")
	}
	gateway, e := s.verifier.VerifySupervisionChallenge(signed, s.target.GatewayPrincipal)
	if e != nil {
		return lease, errors.New("untrusted or expired supervision challenge")
	}
	ch := signed.Challenge
	g := s.client.config.Grant.Grant
	if ch.Tenant != g.Tenant || ch.Site != g.Site || ch.Domain != "simulation" || ch.RunID != s.target.RunID || ch.Audience != s.client.config.Audience || ch.Assignee != s.target.Spec.Assignee || ch.TaskID != s.target.Spec.ID || ch.SpecSHA256 != s.digest || ch.OperatorPrincipal != g.Principal || ch.OperatorKeySHA256 != authority.KeyID(g.PublicKey) || ch.OperatorGrantID != g.ID || ch.RequestID != requestID || ch.ExpiresAtUnixMS > s.target.Spec.NotAfterUnixMS || ch.ExpiresAtUnixMS-ch.IssuedAtUnixMS > SupervisionMaxLease.Milliseconds() {
		return lease, errors.New("supervision challenge binding mismatch")
	}
	if s.boot != "" && (ch.GatewayBootID != s.boot || ch.ExecutorEpoch != s.epoch) {
		return lease, errors.New("gateway boot or executor generation changed; explicit restart required")
	}
	if ctx.Err() != nil {
		return lease, errors.New("supervision challenge deadline exceeded")
	}
	ack, e := authority.SignSupervisionAck(s.client.config.Grant, ed25519.PrivateKey(s.client.config.PrivateKey), supervision.Ack{Challenge: ch})
	if e != nil {
		return lease, errors.New("supervision acknowledgement signing refused")
	}
	body, e := json.Marshal(ack)
	if e != nil {
		return lease, errors.New("invalid supervision acknowledgement")
	}
	wire = nil
	if e = s.client.Do(ctx, "supervise", "POST", "/v1/tasks/supervision/ack", body, &wire); e != nil {
		return lease, supervisionExchangeError("acknowledgement", ctx, e)
	}
	var receipt authority.SignedSupervisionReceipt
	if len(wire) > authority.MaxEnvelopeBytes || authority.DecodeStrict(wire, &receipt) != nil {
		return lease, errors.New("invalid supervision receipt response")
	}
	receiptGateway, e := s.verifier.VerifySupervisionReceipt(receipt, s.target.GatewayPrincipal)
	if e != nil || receiptGateway.SigningKeySHA256 != gateway.SigningKeySHA256 || receiptGateway.GrantID != gateway.GrantID || receipt.Receipt.Challenge != ch || receipt.Receipt.AckSHA256 != authority.SupervisionAckDigest(ack) {
		return lease, errors.New("untrusted, stale or mismatched supervision receipt")
	}
	if ctx.Err() != nil {
		return lease, errors.New("supervision receipt arrived after exchange deadline")
	}
	s.boot, s.epoch = ch.GatewayBootID, ch.ExecutorEpoch
	return SupervisionLease{Schema: "wendy.world.supervision-lease.v1", Domain: "simulation", TaskID: ch.TaskID, SpecSHA256: ch.SpecSHA256, RunID: ch.RunID, OperatorPrincipal: ch.OperatorPrincipal, GatewayPrincipal: ch.GatewayPrincipal, GatewayBootID: ch.GatewayBootID, ExecutorEpoch: ch.ExecutorEpoch, RequestID: ch.RequestID, ChallengeID: ch.ChallengeID, IssuedAtUnixMS: ch.IssuedAtUnixMS, ExpiresAtUnixMS: ch.ExpiresAtUnixMS, AckSHA256: receipt.Receipt.AckSHA256}, nil
}

// Never wrap or stringify cause: HTTP bodies and transport errors can contain
// credentials, signed proofs or endpoint URLs. These fixed classes are enough
// to distinguish a deadline from a server refusal in a supervision run.
func supervisionExchangeError(stage string, ctx context.Context, cause error) error {
	var status *HTTPStatusError
	var transport net.Error
	class := "transport or response"
	switch {
	case errors.As(cause, &status):
		class = fmt.Sprintf("HTTP %d", status.StatusCode)
	case errors.Is(cause, context.Canceled) || ctx.Err() == context.Canceled:
		class = "cancelled"
	case errors.Is(cause, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		class = "timeout"
	case errors.As(cause, &transport) && transport.Timeout():
		class = "timeout"
	}
	return fmt.Errorf("supervision %s exchange failed (%s)", stage, class)
}
