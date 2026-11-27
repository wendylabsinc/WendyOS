package main

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type publicationClient interface {
	PreparePublication(world.Component) (client.PreparedPublication, error)
	PublishPrepared(context.Context, client.PreparedPublication) error
}
type candidate struct {
	component world.Component
	signed    client.PreparedPublication
	input     string
	acked     bool
}

// publicationState keeps one latest intent and the longest-lived claim that
// might have reached a replica. An uncertain POST is still a possible claim and
// must be covered by a later withdrawal. Neither retry nor withdrawal changes
// the original claim's sample time or expiry.
type publicationState struct {
	publisher publicationClient
	epoch     uint64
	sequence  uint64
	current   *candidate
	possible  *world.Component
}

func expiry(c world.Component) time.Time { return c.Timing.SampleTime.Add(c.TTL) }

func (s *publicationState) prepare(component world.Component, input string) error {
	if s.epoch == 0 || s.sequence == math.MaxUint64 {
		return errors.New("invalid or exhausted fusion publisher version")
	}
	s.sequence++
	component.Version = world.Version{Epoch: s.epoch, Sequence: s.sequence}
	prepared, err := s.publisher.PreparePublication(component)
	if err != nil {
		return err
	}
	s.current = &candidate{component: component, signed: prepared, input: input}
	return nil
}

func (s *publicationState) update(now time.Time, component *world.Component, input string) error {
	if s.possible != nil && !now.Before(expiry(*s.possible)) {
		s.possible = nil
	}
	if s.current != nil && !now.Before(expiry(s.current.component)) {
		s.current = nil
	}
	if component != nil && !now.Before(expiry(*component)) {
		component = nil
	}
	if component == nil {
		if s.current != nil && s.current.component.Tombstone {
			return nil
		}
		if s.possible == nil {
			s.current = nil
			return nil
		}
		tombstone := *s.possible
		tombstone.Tombstone = true
		tombstone.Payload = nil
		return s.prepare(tombstone, "")
	}
	if component.Tombstone || input == "" {
		return errors.New("invalid fusion publication intent")
	}
	if s.current != nil && !s.current.component.Tombstone && s.current.input == input && !expiry(*component).Before(expiry(s.current.component)) {
		// New arrival time cannot extend the same inputs' original claim.
		return nil
	}
	return s.prepare(*component, input)
}

func (s *publicationState) send(ctx context.Context, now time.Time) (*world.Component, error) {
	if s.current == nil || s.current.acked {
		return nil, nil
	}
	c := s.current
	if !now.Before(expiry(c.component)) {
		s.current = nil
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.component.Tombstone && (s.possible == nil || expiry(c.component).After(expiry(*s.possible))) {
		possible := c.component
		s.possible = &possible
	}
	if err := s.publisher.PublishPrepared(ctx, c.signed); err != nil {
		return nil, err
	}
	c.acked = true
	if c.component.Tombstone {
		s.possible = nil
	}
	return &c.component, nil
}

type retryDelay struct {
	delay time.Duration
	next  time.Time
}

func (r *retryDelay) failed(now time.Time) {
	if r.delay == 0 {
		r.delay = 400 * time.Millisecond
	} else {
		r.delay = min(5*time.Second, r.delay*2)
	}
	r.next = now.Add(r.delay)
}
func (r *retryDelay) ready(now time.Time) bool { return !now.Before(r.next) }
func (r *retryDelay) reset()                   { *r = retryDelay{} }

type epochClient interface {
	NextEpoch(context.Context) (uint64, error)
}

func awaitEpoch(ctx context.Context, publisher epochClient) (uint64, error) {
	var retry retryDelay
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		epoch, err := publisher.NextEpoch(attempt)
		cancel()
		if err == nil {
			return epoch, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if !client.Transient(err) {
			return 0, err
		}
		retry.failed(time.Now())
		timer := time.NewTimer(retry.delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
}
