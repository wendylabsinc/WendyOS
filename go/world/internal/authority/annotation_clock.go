package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
)

func annotationClockPayload(version int, grant json.RawMessage, message any) any {
	return struct {
		Version     int    `json:"version"`
		GrantSHA256 string `json:"grant_sha256"`
		Message     any    `json:"message"`
	}{version, hash(grant), message}
}
func clockGrant(raw json.RawMessage) (SignedGrant, error) {
	var g SignedGrant
	if len(raw) > 16<<10 || DecodeStrict(raw, &g) != nil {
		return g, ErrDenied
	}
	b, _ := json.Marshal(g)
	if !bytes.Equal(b, raw) {
		return g, ErrDenied
	}
	return g, nil
}
func SignAnnotationClockRequest(g SignedGrant, key ed25519.PrivateKey, r annotationclock.Request) (annotationclock.SignedRequest, error) {
	raw, _ := json.Marshal(g)
	s := annotationclock.SignedRequest{Version: 1, Grant: raw, Request: r}
	if !privateMatches(key, g.Grant.PublicKey) || r.Validate() != nil || g.Grant.Kind != "operator" || g.Grant.Domain != "simulation" || !contains(g.Grant.Scopes, "annotation-write") || g.Grant.PublishAudience != r.Audience || g.Grant.Tenant != r.Tenant || g.Grant.Site != r.Site {
		return s, ErrDenied
	}
	sig, err := sign(key, "annotation-clock-request", annotationClockPayload(s.Version, s.Grant, s.Request))
	s.Signature = sig
	return s, err
}
func validClockRequest(s annotationclock.SignedRequest) (SignedGrant, error) {
	g, err := clockGrant(s.Grant)
	if err != nil {
		return g, err
	}
	r := s.Request
	if s.Version != 1 || r.Validate() != nil || g.Grant.Kind != "operator" || g.Grant.Domain != "simulation" || !contains(g.Grant.Scopes, "annotation-write") || g.Grant.PublishAudience != r.Audience || g.Grant.Tenant != r.Tenant || g.Grant.Site != r.Site || !verify(g.Grant.PublicKey, "annotation-clock-request", annotationClockPayload(s.Version, s.Grant, r), s.Signature) {
		return g, ErrDenied
	}
	return g, nil
}
func validClockServer(g Grant, audience string) bool {
	return g.Kind == "device" && g.Domain == "simulation" && g.PublishAudience == audience && contains(g.Scopes, "annotation-clock-server")
}

// IssueAnnotationClock authenticates against the gateway clock, never a client
// timestamp. It persists the authority floor but consumes no request nonce.
func (v *Verifier) IssueAnnotationClock(request annotationclock.SignedRequest, server SignedGrant, key ed25519.PrivateKey, runID, domain, boot string) (annotationclock.SignedObservation, error) {
	v.lock()
	defer v.mu.Unlock()
	var out annotationclock.SignedObservation
	operator, err := validClockRequest(request)
	if err != nil {
		return out, err
	}
	r := request.Request
	if v.cfg.Domain != "simulation" || r.Tenant != v.cfg.Tenant || r.Site != v.cfg.Site || r.Audience != v.cfg.Audience || r.RunID != runID || r.ClockDomain != domain || !validClockServer(server.Grant, r.Audience) || !privateMatches(key, server.Grant.PublicKey) {
		return out, ErrDenied
	}
	start := v.cfg.Now()
	if _, err = v.identity(operator, "annotation-write", start, start); err != nil {
		return out, err
	}
	if _, err = v.identity(server, "annotation-clock-server", start, start); err != nil {
		return out, err
	}
	now := v.cfg.Now()
	if now.Before(start) || now.Round(0).Before(start.Round(0)) {
		return out, ErrDenied
	}
	// Recheck after any durable work; no new I/O follows these checks.
	if _, _, err = v.validateIdentity(operator, "annotation-write", now, now); err != nil {
		return out, err
	}
	if _, _, err = v.validateIdentity(server, "annotation-clock-server", now, now); err != nil {
		return out, err
	}
	requestBytes, _ := json.Marshal(request)
	raw, _ := json.Marshal(server)
	o := annotationclock.Observation{RequestSHA256: hash(requestBytes), Tenant: r.Tenant, Site: r.Site, Audience: r.Audience, RunID: r.RunID, ClockDomain: r.ClockDomain, Nonce: r.Nonce, OperatorPrincipal: operator.Grant.Principal, OperatorKeySHA256: KeyID(operator.Grant.PublicKey), GatewayPrincipal: server.Grant.Principal, GatewayBootID: boot, SampleUnixMS: now.UnixMilli(), Reference: annotationclock.Reference, MaxRTTMS: annotationclock.MaxRTT.Milliseconds(), MaxHoldoverMS: annotationclock.MaxHoldover.Milliseconds(), DriftPPM: annotationclock.DriftPPM, QuantizationMS: annotationclock.QuantizationMS}
	if o.Validate() != nil {
		return out, ErrDenied
	}
	out = annotationclock.SignedObservation{Version: 1, Grant: raw, Observation: o}
	out.Signature, err = sign(key, "annotation-clock-observation", annotationClockPayload(out.Version, out.Grant, o))
	return out, err
}

// VerifyAnnotationClockObservation uses explicit pins and a matching outstanding
// nonce. It does not infer UTC accuracy or current permission from client time.
func VerifyAnnotationClockObservation(request annotationclock.SignedRequest, response annotationclock.SignedObservation, pins annotationclock.Pins) (annotationclock.Observation, error) {
	o := response.Observation
	operator, server, err := verifyClockPair(request, response, ed25519.PublicKey(pins.RootPublicKey))
	if err != nil {
		return o, err
	}
	_ = operator
	if len(pins.RootPublicKey) != ed25519.PublicKeySize || o.GatewayPrincipal != pins.GatewayPrincipal || KeyID(server.Grant.PublicKey) != pins.GatewayKeySHA256 || o.RunID != pins.RunID || o.ClockDomain != pins.ClockDomain {
		return o, ErrDenied
	}
	return o, nil
}
func verifyClockPair(request annotationclock.SignedRequest, response annotationclock.SignedObservation, root ed25519.PublicKey) (SignedGrant, SignedGrant, error) {
	operator, err := validClockRequest(request)
	if err != nil {
		return operator, SignedGrant{}, err
	}
	server, err := clockGrant(response.Grant)
	if err != nil {
		return operator, server, err
	}
	o := response.Observation
	r := request.Request
	raw, _ := json.Marshal(request)
	if response.Version != 1 || o.Validate() != nil || o.RequestSHA256 != hash(raw) || o.Tenant != r.Tenant || o.Site != r.Site || o.Audience != r.Audience || o.RunID != r.RunID || o.ClockDomain != r.ClockDomain || o.Nonce != r.Nonce || o.OperatorPrincipal != operator.Grant.Principal || o.OperatorKeySHA256 != KeyID(operator.Grant.PublicKey) || o.GatewayPrincipal != server.Grant.Principal || server.Grant.Tenant != r.Tenant || server.Grant.Site != r.Site || !validClockServer(server.Grant, r.Audience) || !verify(server.Grant.PublicKey, "annotation-clock-observation", annotationClockPayload(response.Version, response.Grant, o), response.Signature) {
		return operator, server, ErrDenied
	}
	for _, g := range []SignedGrant{operator, server} {
		if validateGrant(g.Grant, 365*24*time.Hour) != nil || !verify(root, "grant", g.Grant, g.Signature) || o.SampleUnixMS < g.Grant.NotBeforeUnixMS || o.SampleUnixMS >= g.Grant.NotAfterUnixMS {
			return operator, server, ErrDenied
		}
	}
	return operator, server, nil
}

// Called with verifier mutex held. Historical checks never authorize visibility.
func (v *Verifier) annotationClockRecord(r SignedRecord, now time.Time, history bool) error {
	if annotations.IsAmendment(r.Component) && !history {
		a, e := annotations.ValidateAmendment(r.Component)
		if e != nil {
			return e
		}
		if now.UnixMilli() < a.AuthoredAtUnixMS || now.Add(r.Component.Timing.ErrorBound).UnixMilli() >= a.ExpiresAtUnixMS {
			return ErrDenied
		}
		if _, _, e = v.validateIdentity(r.Grant, "annotation-write", now, now); e != nil {
			return e
		}
	}

	var note struct {
		Clock *annotationclock.Evidence `json:"clock"`
	}
	if json.Unmarshal(r.Component.Payload, &note) != nil {
		return ErrDenied
	}
	if note.Clock == nil {
		return nil
	}
	e := note.Clock
	operator, server, err := verifyClockPair(e.Request, e.Observation, v.cfg.RootPublicKey)
	if err != nil {
		return err
	}
	o := e.Observation.Observation
	g := r.Grant.Grant
	interval, err := e.Interval()
	if err != nil {
		return err
	}
	if operator.Grant.Principal != g.Principal || !bytes.Equal(operator.Grant.PublicKey, g.PublicKey) || operator.Grant.Source != g.Source || o.Tenant != g.Tenant || o.Site != g.Site || o.Audience != g.PublishAudience || o.ClockDomain != r.Component.Timing.ClockDomain || interval.LowerUnixMS < g.NotBeforeUnixMS || interval.UpperUnixMS >= g.NotAfterUnixMS {
		return ErrDenied
	}
	if !history {
		if o.SampleUnixMS > now.UnixMilli() {
			return ErrDenied
		}
		if _, _, err = v.validateIdentity(operator, "annotation-write", time.UnixMilli(o.SampleUnixMS), now); err != nil {
			return err
		}
		if _, _, err = v.validateIdentity(server, "annotation-clock-server", time.UnixMilli(o.SampleUnixMS), now); err != nil {
			return err
		}
	}
	return nil
}
