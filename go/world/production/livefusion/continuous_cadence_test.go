package livefusion

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
)

// Exercise the real continuous session and its empty-source path. Direct
// fixture calls keep network scheduling out of deterministic cadence checks.
type cadenceClient struct {
	appv1.WorldAppServiceClient
	fixture *fixtureServer
	delays  []time.Duration
	starts  []time.Time
	active  int
	maximum int
}

func (c *cadenceClient) Clock(ctx context.Context, request *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	return c.fixture.Clock(ctx, request)
}

func (c *cadenceClient) Query(ctx context.Context, _ *appv1.QueryRequest, _ ...grpc.CallOption) (*appv1.QueryReply, error) {
	c.starts = append(c.starts, time.Now())
	c.active++
	if c.active > c.maximum {
		c.maximum = c.active
	}
	defer func() { c.active-- }()
	timer := time.NewTimer(c.delays[len(c.starts)-1])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return &appv1.QueryReply{}, nil
	}
}

func TestContinuousCadenceUsesElapsedEvaluationAndEmission(t *testing.T) {
	fixture, _ := setup(t, "")
	fixture.cfg.MaxTicks = 3
	fixture.cfg.RequestTimeoutMS = 3000
	synctest.Test(t, func(t *testing.T) {
		client := &cadenceClient{fixture: fixture, delays: []time.Duration{100 * time.Millisecond, 1200 * time.Millisecond, 100 * time.Millisecond}}
		started := time.Now()
		emitted := 0
		out, err := RunContinuous(context.Background(), fixture.cfg, client, func(r TickResult) error {
			emitted++
			if r.Tick != emitted || r.Status != "insufficient_evidence" {
				t.Fatalf("tick %d: %+v", emitted, r)
			}
			time.Sleep(50 * time.Millisecond)
			return nil
		})
		if err != nil || out.Status != "complete" || out.Ticks != 3 || out.Published != 0 || client.maximum != 1 {
			t.Fatalf("continuous result: %+v, %v; active maximum %d", out, err, client.maximum)
		}
		want := []time.Duration{0, 500 * time.Millisecond, 1750 * time.Millisecond}
		for i, at := range client.starts {
			if got := at.Sub(started); got != want[i] {
				t.Fatalf("tick %d start = %s, want %s", i+1, got, want[i])
			}
		}
		if elapsed := time.Since(started); elapsed != 1900*time.Millisecond {
			t.Fatalf("added delay after final tick: %s", elapsed)
		}
	})
}

func TestContinuousCadenceCancellationDuringRemainingWait(t *testing.T) {
	fixture, _ := setup(t, "")
	fixture.cfg.MaxTicks = 3
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cadenceClient{fixture: fixture, delays: []time.Duration{10 * time.Millisecond}}
		started := time.Now()
		out, err := RunContinuous(ctx, fixture.cfg, client, func(TickResult) error {
			go func() { time.Sleep(100 * time.Millisecond); cancel() }()
			return nil
		})
		if !errors.Is(err, context.Canceled) || out.Ticks != 1 || len(client.starts) != 1 || time.Since(started) != 110*time.Millisecond {
			t.Fatalf("cancellation: %+v, %v, starts %d, elapsed %s", out, err, len(client.starts), time.Since(started))
		}
	})
}

func TestContinuousCadenceCanceledSlowTickDoesNotStartAnother(t *testing.T) {
	fixture, _ := setup(t, "")
	fixture.cfg.MaxTicks = 3
	fixture.cfg.RequestTimeoutMS = 3000
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &cadenceClient{fixture: fixture, delays: []time.Duration{time.Second}}
		out, err := RunContinuous(ctx, fixture.cfg, client, func(TickResult) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) || out.Ticks != 1 || len(client.starts) != 1 {
			t.Fatalf("canceled slow tick: %+v, %v, starts %d", out, err, len(client.starts))
		}
	})
}
