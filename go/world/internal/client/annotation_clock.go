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

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// AnnotationClock is an in-process estimate, not a persistent or UTC clock.
// The caller supplies a suspend-inclusive monotonic clock and must discard this
// object on boot change/regression. No wall-clock fallback is provided.
type AnnotationClock struct {
	mu         sync.Mutex
	last       time.Duration
	invalid    bool
	evidence   annotationclock.Evidence
	received   time.Duration
	elapsedNow func() time.Duration
	scope      string
}

func (c *Client) ObserveAnnotationClock(ctx context.Context, elapsedNow func() time.Duration) (*AnnotationClock, error) {
	if c.config.ClockTrust == nil || elapsedNow == nil {
		return nil, errors.New("annotation clock pins and suspend-inclusive monotonic provider required")
	}
	pins := *c.config.ClockTrust
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	g := c.config.Grant.Grant
	request, err := authority.SignAnnotationClockRequest(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), annotationclock.Request{Tenant: g.Tenant, Site: g.Site, Audience: c.config.Audience, RunID: pins.RunID, ClockDomain: pins.ClockDomain, Nonce: hex.EncodeToString(nonce[:])})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(request)
	ctx, cancel := context.WithTimeout(ctx, annotationclock.MaxRTT)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL+"/v1/annotations/clock", bytes.NewReader(body))
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
		return nil, &HTTPStatusError{StatusCode: response.StatusCode, message: "annotation clock unavailable"}
	}
	var observation annotationclock.SignedObservation
	if authority.DecodeStrict(raw, &observation) != nil {
		return nil, errors.New("invalid clock observation")
	}
	if _, err = authority.VerifyAnnotationClockObservation(request, observation, pins); err != nil {
		return nil, err
	}
	clock := &AnnotationClock{evidence: annotationclock.Evidence{Request: request, Observation: observation, RTTNS: int64(received - start)}, received: received, last: received, elapsedNow: elapsedNow, scope: c.annotationClockScope()}
	if start < 0 || received < start {
		return nil, errors.New("monotonic clock regressed")
	}
	if _, err = clock.Evidence(); err != nil {
		return nil, err
	}
	return clock, nil
}
func (c *Client) annotationClockScope() string {
	b, _ := json.Marshal(struct {
		Audience string
		Grant    authority.SignedGrant
		Pins     *annotationclock.Pins
	}{c.config.Audience, c.config.Grant, c.config.ClockTrust})
	return authority.KeyID(b)
}

// Evidence returns a new immutable copy for immediate draft persistence. It
// never changes an earlier draft's evidence, timestamps, deadline or signature.
func (c *AnnotationClock) Evidence() (annotationclock.Evidence, error) {
	if c == nil || c.elapsedNow == nil {
		return annotationclock.Evidence{}, errors.New("annotation clock unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid {
		return annotationclock.Evidence{}, errors.New("annotation clock requires a fresh observation")
	}
	now := c.elapsedNow()
	if now < c.received || now < c.last {
		c.invalid = true
		return annotationclock.Evidence{}, errors.New("monotonic clock regressed")
	}
	raw, _ := json.Marshal(c.evidence)
	var e annotationclock.Evidence
	_ = json.Unmarshal(raw, &e)
	e.ElapsedNS = int64(now - c.received)
	if _, err := e.Interval(); err != nil {
		c.invalid = true
		return annotationclock.Evidence{}, err
	}
	c.last = now
	return e, nil
}
func (c *Client) doAnnotationClock(ctx context.Context, clock *AnnotationClock, path string, body []byte, out any) error {
	if clock == nil || clock.scope != c.annotationClockScope() || (path != "/v1/annotations" && path != "/v1/annotations/epoch") {
		return errors.New("annotation clock scope mismatch")
	}
	e, err := clock.Evidence()
	if err != nil {
		return err
	}
	interval, err := e.Interval()
	if err != nil {
		return err
	}
	g := c.config.Grant.Grant
	expiry := interval.LowerUnixMS + 60000
	if expiry > g.NotAfterUnixMS {
		expiry = g.NotAfterUnixMS
	}
	if interval.LowerUnixMS < g.NotBeforeUnixMS || interval.UpperUnixMS >= expiry {
		return errors.New("annotation request clock interval outside grant")
	}
	return c.doAt(ctx, "annotation-write", http.MethodPost, path, body, out, time.UnixMilli(interval.LowerUnixMS), time.Duration(expiry-interval.LowerUnixMS)*time.Millisecond)
}
func (c *Client) NextAnnotationEpochWithClock(ctx context.Context, clock *AnnotationClock) (uint64, error) {
	var r struct {
		Epoch uint64 `json:"epoch"`
	}
	err := c.doAnnotationClock(ctx, clock, "/v1/annotations/epoch", []byte("{}"), &r)
	if err == nil && r.Epoch == 0 {
		err = errors.New("zero annotation epoch")
	}
	return r.Epoch, err
}
func (c *Client) PublishAnnotationWithClock(ctx context.Context, clock *AnnotationClock, body []byte) (AnnotationResult, error) {
	return c.publishAnnotation(ctx, body, func(out any) error { return c.doAnnotationClock(ctx, clock, "/v1/annotations", body, out) })
}
func (c *Client) validateClockComponent(componentNote annotations.Note) error {
	if componentNote.Clock == nil {
		return nil
	}
	if c.config.ClockTrust == nil {
		return errors.New("v2 annotations require commissioned clock pins")
	}
	e := componentNote.Clock
	_, err := authority.VerifyAnnotationClockObservation(e.Request, e.Observation, *c.config.ClockTrust)
	return err
}
