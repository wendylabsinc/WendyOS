package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/missionclock"
)

// MissionClock is an in-process estimate, not a persistent or UTC clock.
// The caller supplies a suspend-inclusive monotonic clock and must discard this
// object on boot change/regression. No wall-clock fallback is provided.
type MissionClock struct {
	mu         sync.Mutex
	last       time.Duration
	invalid    bool
	evidence   missionclock.Evidence
	received   time.Duration
	elapsedNow func() time.Duration
	scope      string
	pins       missionclock.Pins
	purpose    string
}

func (c *Client) ObserveMissionClock(ctx context.Context, pins missionclock.Pins, purpose string, elapsedNow func() time.Duration) (*MissionClock, error) {
	if elapsedNow == nil || (purpose != "mission" && purpose != "stop") {
		return nil, errors.New("mission clock pins and suspend-inclusive monotonic provider required")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	g := c.config.Grant.Grant
	request, err := authority.SignMissionClockRequest(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), missionclock.Request{Tenant: g.Tenant, Site: g.Site, Audience: c.config.Audience, RunID: pins.RunID, ClockDomain: pins.ClockDomain, Purpose: purpose, Nonce: hex.EncodeToString(nonce[:])})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(ctx, missionclock.MaxRTT)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL+"/v1/tasks/clock", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	start := elapsedNow()
	response, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	received := elapsedNow()
	if err != nil {
		return nil, err
	}
	if len(raw) > 16<<10 {
		return nil, errors.New("oversized clock response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: response.StatusCode, message: "mission clock unavailable"}
	}
	var observation missionclock.SignedObservation
	if authority.DecodeStrict(raw, &observation) != nil {
		return nil, errors.New("invalid clock observation")
	}
	if _, err = authority.VerifyMissionClockObservation(request, observation, pins); err != nil {
		return nil, err
	}
	clock := &MissionClock{evidence: missionclock.Evidence{Request: request, Observation: observation, RTTNS: int64(received - start)}, received: received, last: received, elapsedNow: elapsedNow, scope: c.missionClockScope(pins, purpose), pins: pins, purpose: purpose}
	if start < 0 || received < start {
		return nil, errors.New("monotonic clock regressed")
	}
	if _, err = clock.Evidence(); err != nil {
		return nil, err
	}
	return clock, nil
}
func (c *Client) missionClockScope(pins missionclock.Pins, purpose string) string {
	b, _ := json.Marshal(struct {
		URL      string
		Audience string
		Grant    authority.SignedGrant
		Pins     missionclock.Pins
		Purpose  string
	}{c.config.URL, c.config.Audience, c.config.Grant, pins, purpose})
	return authority.KeyID(b)
}

// Evidence returns a copy of the transient proof. It must not be persisted as
// a control clock or reconstructed after restart.
func (c *MissionClock) Evidence() (missionclock.Evidence, error) {
	if c == nil || c.elapsedNow == nil {
		return missionclock.Evidence{}, errors.New("mission clock unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid {
		return missionclock.Evidence{}, errors.New("mission clock requires a fresh observation")
	}
	now := c.elapsedNow()
	if now < c.received || now < c.last {
		c.invalid = true
		return missionclock.Evidence{}, errors.New("monotonic clock regressed")
	}
	raw, _ := json.Marshal(c.evidence)
	var e missionclock.Evidence
	_ = json.Unmarshal(raw, &e)
	e.ElapsedNS = int64(now - c.received)
	if _, err := e.Interval(); err != nil {
		c.invalid = true
		return missionclock.Evidence{}, err
	}
	c.last = now
	return e, nil
}

func (c *MissionClock) Interval() (missionclock.Interval, error) {
	e, err := c.Evidence()
	if err != nil {
		return missionclock.Interval{}, err
	}
	return e.Interval()
}

// DoWithMissionClock signs only transport bytes. Callers persist and reuse the
// same independently signed instruction; this function never rebases it.
func (c *Client) DoWithMissionClock(ctx context.Context, clock *MissionClock, operation, method, path string, body []byte, out any) error {
	if clock == nil || clock.scope != c.missionClockScope(clock.pins, clock.purpose) || (operation != clock.purpose && !(operation == "read" && clock.purpose == "mission")) {
		return errors.New("mission clock scope mismatch")
	}
	if operation == "mission" && (method != http.MethodPost || path != "/v1/tasks") || operation == "stop" && (method != http.MethodPost || path != "/v1/tasks/stop") || operation == "read" && method != http.MethodGet {
		return errors.New("mission clock endpoint mismatch")
	}
	interval, err := clock.Interval()
	if err != nil {
		return err
	}
	g := c.config.Grant.Grant
	expiry := interval.LowerUnixMS + 60000
	if operation != "stop" && expiry > g.NotAfterUnixMS {
		expiry = g.NotAfterUnixMS
	}
	if interval.LowerUnixMS < g.NotBeforeUnixMS || interval.UpperUnixMS >= expiry {
		return errors.New("mission clock interval outside authority")
	}
	return c.doAt(ctx, operation, method, path, body, out, time.UnixMilli(interval.LowerUnixMS), time.Duration(expiry-interval.LowerUnixMS)*time.Millisecond)
}
