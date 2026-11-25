package client

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
	"os"
	"testing"
	"time"
)

func nativeClockFixture(t *testing.T) (*Client, *MissionClock, *time.Duration) {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/native-mission-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Grant       authority.SignedGrant          `json:"operator_grant"`
		Key         []byte                         `json:"operator_private_key"`
		Pins        missionclock.Pins              `json:"mission_clock_trust"`
		Request     missionclock.SignedRequest     `json:"clock_request"`
		Observation missionclock.SignedObservation `json:"clock_observation"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: "http://localhost:9999", Audience: "node-1", Grant: fixture.Grant, PrivateKey: fixture.Key, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Second
	clock := &MissionClock{evidence: missionclock.Evidence{Request: fixture.Request, Observation: fixture.Observation, RTTNS: int64(20 * time.Millisecond)}, received: elapsed, last: elapsed, elapsedNow: func() time.Duration { return elapsed }, pins: fixture.Pins, purpose: "mission", scope: c.missionClockScope(fixture.Pins, "mission")}
	return c, clock, &elapsed
}
func TestMissionClockHoldoverRegressionAndImmutableEvidence(t *testing.T) {
	for _, kind := range []string{"holdover", "regression"} {
		t.Run(kind, func(t *testing.T) {
			_, clock, elapsed := nativeClockFixture(t)
			e, err := clock.Evidence()
			if err != nil {
				t.Fatal(err)
			}
			e.Observation.Observation.SampleUnixMS = 1
			if _, err = clock.Interval(); err != nil {
				t.Fatal("caller changed retained evidence", err)
			}
			if kind == "holdover" {
				*elapsed += missionclock.MaxHoldover + 1
			} else {
				*elapsed -= 1
			}
			if _, err = clock.Interval(); err == nil {
				t.Fatal("invalid monotonic interval accepted")
			}
			*elapsed = time.Second
			if _, err = clock.Interval(); err == nil {
				t.Fatal("invalidated clock became valid again")
			}
		})
	}
}
func TestMissionClockCannotAuthorizeStopOrOtherProfile(t *testing.T) {
	c, clock, _ := nativeClockFixture(t)
	for _, op := range []string{"stop", "annotation-write", "execute"} {
		if err := c.DoWithMissionClock(context.Background(), clock, op, "POST", "/v1/tasks/stop", nil, nil); err == nil {
			t.Fatal("clock authorized wrong purpose")
		}
	}
	c.config.Audience = "other"
	if err := c.DoWithMissionClock(context.Background(), clock, "mission", "POST", "/v1/tasks", nil, nil); err == nil {
		t.Fatal("clock crossed audience")
	}
	c, clock, _ = nativeClockFixture(t)
	c.config.URL = "http://localhost:9998"
	if err := c.DoWithMissionClock(context.Background(), clock, "mission", "POST", "/v1/tasks", nil, nil); err == nil {
		t.Fatal("clock crossed endpoint")
	}
}
