package main

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type capturePreparer interface {
	PreparePublication(world.Component) (client.PreparedPublication, error)
}

// A repeated capture after a lost response retries its exact origin record.
// The capture client rejects mutations within one boot/sequence, and the caller
// checks capture age again before every send. Only one capture is retained.
type capturePublication struct {
	sequence uint64
	signed   client.PreparedPublication
}

func (p *capturePublication) prepare(c capturePreparer, sequence uint64, component world.Component) error {
	if sequence == 0 || sequence < p.sequence {
		return errors.New("capture publication sequence regressed")
	}
	if sequence == p.sequence {
		return nil
	}
	signed, err := c.PreparePublication(component)
	if err != nil {
		return err
	}
	p.sequence, p.signed = sequence, signed
	return nil
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
