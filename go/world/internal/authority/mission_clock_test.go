package authority

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
)

func missionClockFixture(t *testing.T, purpose string) (*fixture, SignedGrant, ed25519.PrivateKey, missionclock.SignedRequest, missionclock.SignedObservation, missionclock.Pins) {
	t.Helper()
	f := newFixture(t, nil)
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"read", "mission", "stop"}
	g.PublishAudience = ""
	f.grant, _ = SignGrant(g, f.root)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	g.Kind = "device"
	g.ID = "mission-clock"
	g.Principal = "gateway-clock"
	g.PublishAudience = "node-1"
	g.PublicKey = key.Public().(ed25519.PublicKey)
	g.Scopes = []string{"mission-clock-server"}
	server, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	req, err := SignMissionClockRequest(f.grant, f.key, missionclock.Request{Tenant: g.Tenant, Site: g.Site, Audience: "node-1", RunID: "run", ClockDomain: "simulation:run", Purpose: purpose, Nonce: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := f.verifier.IssueMissionClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	pins := missionclock.Pins{RootPublicKey: f.root.Public().(ed25519.PublicKey), GatewayPrincipal: g.Principal, GatewayKeySHA256: KeyID(g.PublicKey), RunID: "run", ClockDomain: "simulation:run"}
	return f, server, key, req, obs, pins
}
func TestMissionClockPurposeAndExpiredStopAuthority(t *testing.T) {
	for _, purpose := range []string{"mission", "stop"} {
		t.Run(purpose, func(t *testing.T) {
			f, server, key, req, obs, pins := missionClockFixture(t, purpose)
			if _, err := VerifyMissionClockObservation(req, obs, pins); err != nil {
				t.Fatal(err)
			}
			g := f.grant.Grant
			g.NotAfterUnixMS = f.now.Add(-time.Second).UnixMilli()
			f.grant, _ = SignGrant(g, f.root)
			req, err := SignMissionClockRequest(f.grant, f.key, req.Request)
			if err != nil {
				t.Fatal(err)
			}
			obs, err = f.verifier.IssueMissionClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32))
			if purpose == "mission" {
				if err == nil {
					t.Fatal("expired mission grant accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("expired stop denied", err)
			}
			if _, err = VerifyMissionClockObservation(req, obs, pins); err != nil {
				t.Fatal("client rejected valid expired stop", err)
			}
			g.NotBeforeUnixMS = f.now.Add(time.Second).UnixMilli()
			g.NotAfterUnixMS = f.now.Add(time.Hour).UnixMilli()
			f.grant, _ = SignGrant(g, f.root)
			req, _ = SignMissionClockRequest(f.grant, f.key, req.Request)
			if _, err = f.verifier.IssueMissionClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err == nil {
				t.Fatal("future stop authority accepted")
			}
		})
	}
}
func TestMissionClockForgeryAndCrossPurpose(t *testing.T) {
	_, _, _, req, obs, pins := missionClockFixture(t, "mission")
	for name, mutate := range map[string]func(*missionclock.SignedRequest, *missionclock.SignedObservation, *missionclock.Pins){
		"purpose": func(r *missionclock.SignedRequest, _ *missionclock.SignedObservation, _ *missionclock.Pins) {
			r.Request.Purpose = "stop"
		},
		"nonce": func(r *missionclock.SignedRequest, _ *missionclock.SignedObservation, _ *missionclock.Pins) {
			r.Request.Nonce = strings.Repeat("e", 64)
		},
		"sample": func(_ *missionclock.SignedRequest, o *missionclock.SignedObservation, _ *missionclock.Pins) {
			o.Observation.SampleUnixMS++
		},
		"holdover": func(_ *missionclock.SignedRequest, o *missionclock.SignedObservation, _ *missionclock.Pins) {
			o.Observation.MaxHoldoverMS = 3600000
		},
		"key": func(_ *missionclock.SignedRequest, _ *missionclock.SignedObservation, p *missionclock.Pins) {
			p.GatewayKeySHA256 = strings.Repeat("0", 64)
		},
		"run": func(_ *missionclock.SignedRequest, _ *missionclock.SignedObservation, p *missionclock.Pins) {
			p.RunID = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(req)
			var r missionclock.SignedRequest
			_ = json.Unmarshal(raw, &r)
			raw, _ = json.Marshal(obs)
			var o missionclock.SignedObservation
			_ = json.Unmarshal(raw, &o)
			p := pins
			mutate(&r, &o, &p)
			if _, err := VerifyMissionClockObservation(r, o, p); err == nil {
				t.Fatal("forged observation accepted")
			}
		})
	}
	f, server, key, r, _, _ := missionClockFixture(t, "mission")
	r.Signature, _ = sign(f.key, "annotation-clock-request", missionClockPayload(r.Version, r.Grant, r.Request))
	if _, err := f.verifier.IssueMissionClock(r, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err == nil {
		t.Fatal("annotation proof reused")
	}
}
func TestMissionClockRevocationAndCompletionFence(t *testing.T) {
	for _, kind := range []string{"operator", "operator-key", "server", "stale", "postwrite", "write-failure"} {
		t.Run(kind, func(t *testing.T) {
			f, server, key, req, _, _ := missionClockFixture(t, "stop")
			r := f.rev.Revocations
			r.Revision++
			switch kind {
			case "operator":
				r.Principals = []string{f.grant.Grant.Principal}
			case "operator-key":
				r.KeyIDs = []string{KeyID(f.grant.Grant.PublicKey)}
			case "server":
				r.GrantIDs = []string{server.Grant.ID}
			case "stale":
				f.now = f.now.Add(time.Hour)
			case "postwrite":
				f.verifier.cfg.Replay = &clockAdvanceReplay{ReplayStore: f.verifier.cfg.Replay, after: func() { f.now = f.now.Add(2 * time.Hour) }}
			case "write-failure":
				f.verifier.cfg.Replay = &clockAdvanceReplay{ReplayStore: f.verifier.cfg.Replay, failure: errors.New("disk unavailable")}
			}
			if kind == "operator" || kind == "operator-key" || kind == "server" {
				signed, _ := SignRevocations(r, f.root)
				if err := f.verifier.UpdateRevocations(signed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.verifier.IssueMissionClock(req, server, key, "run", "simulation:run", strings.Repeat("cd", 32)); err == nil {
				t.Fatal("unavailable authority issued clock")
			}
		})
	}
}
