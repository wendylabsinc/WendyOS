package authority

import (
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
)

func missionClockPayload(version int, grant json.RawMessage, message any) any {
	return struct {
		Version     int    `json:"version"`
		GrantSHA256 string `json:"grant_sha256"`
		Message     any    `json:"message"`
	}{version, hash(grant), message}
}
func SignMissionClockRequest(g SignedGrant, key ed25519.PrivateKey, r missionclock.Request) (missionclock.SignedRequest, error) {
	raw, _ := json.Marshal(g)
	s := missionclock.SignedRequest{Version: 1, Grant: raw, Request: r}
	if !privateMatches(key, g.Grant.PublicKey) || r.Validate() != nil || g.Grant.Kind != "operator" || g.Grant.Domain != "simulation" || !contains(g.Grant.Scopes, r.Purpose) || g.Grant.Tenant != r.Tenant || g.Grant.Site != r.Site {
		return s, ErrDenied
	}
	sig, err := sign(key, "mission-clock-request", missionClockPayload(s.Version, s.Grant, s.Request))
	s.Signature = sig
	return s, err
}
func validMissionClockRequest(s missionclock.SignedRequest) (SignedGrant, error) {
	g, err := clockGrant(s.Grant)
	if err != nil {
		return g, err
	}
	r := s.Request
	if s.Version != 1 || r.Validate() != nil || g.Grant.Kind != "operator" || g.Grant.Domain != "simulation" || !contains(g.Grant.Scopes, r.Purpose) || g.Grant.Tenant != r.Tenant || g.Grant.Site != r.Site || !verify(g.Grant.PublicKey, "mission-clock-request", missionClockPayload(s.Version, s.Grant, r), s.Signature) {
		return g, ErrDenied
	}
	return g, nil
}
func validMissionClockServer(g Grant, audience string) bool {
	return g.Kind == "device" && g.Domain == "simulation" && g.PublishAudience == audience && contains(g.Scopes, "mission-clock-server")
}

// IssueMissionClock authenticates against the gateway clock, never a client
// timestamp. It persists the authority floor but consumes no request nonce.
func (v *Verifier) IssueMissionClock(request missionclock.SignedRequest, server SignedGrant, key ed25519.PrivateKey, runID, domain, boot string) (missionclock.SignedObservation, error) {
	v.lock()
	defer v.mu.Unlock()
	var out missionclock.SignedObservation
	operator, err := validMissionClockRequest(request)
	if err != nil {
		return out, err
	}
	r := request.Request
	if v.cfg.Domain != "simulation" || r.Tenant != v.cfg.Tenant || r.Site != v.cfg.Site || r.Audience != v.cfg.Audience || r.RunID != runID || r.ClockDomain != domain || !validMissionClockServer(server.Grant, r.Audience) || !privateMatches(key, server.Grant.PublicKey) {
		return out, ErrDenied
	}
	start := v.cfg.Now()
	if _, err = v.identity(operator, r.Purpose, start, start); err != nil {
		return out, err
	}
	if _, err = v.identity(server, "mission-clock-server", start, start); err != nil {
		return out, err
	}
	now := v.cfg.Now()
	if now.Before(start) || now.Round(0).Before(start.Round(0)) {
		return out, ErrDenied
	}
	// Recheck after any durable work; no new I/O follows these checks.
	if _, _, err = v.validateIdentity(operator, r.Purpose, now, now); err != nil {
		return out, err
	}
	if _, _, err = v.validateIdentity(server, "mission-clock-server", now, now); err != nil {
		return out, err
	}
	requestBytes, _ := json.Marshal(request)
	raw, _ := json.Marshal(server)
	o := missionclock.Observation{RequestSHA256: hash(requestBytes), Tenant: r.Tenant, Site: r.Site, Audience: r.Audience, RunID: r.RunID, ClockDomain: r.ClockDomain, Purpose: r.Purpose, Nonce: r.Nonce, OperatorPrincipal: operator.Grant.Principal, OperatorKeySHA256: KeyID(operator.Grant.PublicKey), GatewayPrincipal: server.Grant.Principal, GatewayBootID: boot, SampleUnixMS: now.UnixMilli(), Reference: missionclock.Reference, MaxRTTMS: missionclock.MaxRTT.Milliseconds(), MaxHoldoverMS: missionclock.MaxHoldover.Milliseconds(), DriftPPM: missionclock.DriftPPM, QuantizationMS: missionclock.QuantizationMS}
	if o.Validate() != nil {
		return out, ErrDenied
	}
	out = missionclock.SignedObservation{Version: 1, Grant: raw, Observation: o}
	out.Signature, err = sign(key, "mission-clock-observation", missionClockPayload(out.Version, out.Grant, o))
	return out, err
}

// VerifyMissionClockObservation uses explicit pins and a matching outstanding
// nonce. It does not infer UTC accuracy or current permission from client time.
func VerifyMissionClockObservation(request missionclock.SignedRequest, response missionclock.SignedObservation, pins missionclock.Pins) (missionclock.Observation, error) {
	o := response.Observation
	operator, server, err := verifyMissionClockPair(request, response, ed25519.PublicKey(pins.RootPublicKey))
	if err != nil {
		return o, err
	}
	_ = operator
	if len(pins.RootPublicKey) != ed25519.PublicKeySize || o.GatewayPrincipal != pins.GatewayPrincipal || KeyID(server.Grant.PublicKey) != pins.GatewayKeySHA256 || o.RunID != pins.RunID || o.ClockDomain != pins.ClockDomain {
		return o, ErrDenied
	}
	return o, nil
}
func verifyMissionClockPair(request missionclock.SignedRequest, response missionclock.SignedObservation, root ed25519.PublicKey) (SignedGrant, SignedGrant, error) {
	operator, err := validMissionClockRequest(request)
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
	if response.Version != 1 || o.Validate() != nil || o.RequestSHA256 != hash(raw) || o.Tenant != r.Tenant || o.Site != r.Site || o.Audience != r.Audience || o.RunID != r.RunID || o.ClockDomain != r.ClockDomain || o.Purpose != r.Purpose || o.Nonce != r.Nonce || o.OperatorPrincipal != operator.Grant.Principal || o.OperatorKeySHA256 != KeyID(operator.Grant.PublicKey) || o.GatewayPrincipal != server.Grant.Principal || server.Grant.Tenant != r.Tenant || server.Grant.Site != r.Site || !validMissionClockServer(server.Grant, r.Audience) || !verify(server.Grant.PublicKey, "mission-clock-observation", missionClockPayload(response.Version, response.Grant, o), response.Signature) {
		return operator, server, ErrDenied
	}
	for index, g := range []SignedGrant{operator, server} {
		if validateGrant(g.Grant, 365*24*time.Hour) != nil || !verify(root, "grant", g.Grant, g.Signature) || o.SampleUnixMS < g.Grant.NotBeforeUnixMS || (o.SampleUnixMS >= g.Grant.NotAfterUnixMS && !(index == 0 && r.Purpose == "stop")) {
			return operator, server, ErrDenied
		}
	}
	return operator, server, nil
}
