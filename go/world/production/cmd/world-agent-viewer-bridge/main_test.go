package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/peoplereader"
	"github.com/wendylabsinc/wendy/go/world/production/viewerbridge"
)

func TestCommandContinuousLifetimeUsesParentCancellation(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	cfg := viewerbridge.Config{People: peoplereader.Config{DurationMS: 1}}
	bounded, finishBounded := commandContext(parent, cfg)
	defer finishBounded()
	cfg.Continuous = true
	continuous, finishContinuous := commandContext(parent, cfg)
	defer finishContinuous()
	if _, ok := bounded.Deadline(); !ok {
		t.Fatal("bounded command lost its duration")
	}
	if _, ok := continuous.Deadline(); ok {
		t.Fatal("continuous command still has a duration deadline")
	}
	select {
	case <-bounded.Done():
	case <-time.After(time.Second):
		t.Fatal("bounded command did not finish")
	}
	if !errors.Is(bounded.Err(), context.DeadlineExceeded) || continuous.Err() != nil {
		t.Fatal("continuous command stopped at the old duration")
	}
	stop()
	if !errors.Is(continuous.Err(), context.Canceled) {
		t.Fatal("continuous command did not follow parent cancellation")
	}
}
