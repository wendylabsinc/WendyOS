package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUDPPolicyRepairJoinsInFlightWorkBeforeRetirement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	finishRepair := sync.OnceFunc(func() { close(release) })
	defer finishRepair()
	var calls, reports atomic.Int32
	failure := errors.New("repair failed")
	done := repairMeshUDPPolicy(ctx, time.Millisecond, func() error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return failure
	}, func(err error) {
		if !errors.Is(err, failure) {
			t.Errorf("repair error changed: %v", err)
		}
		reports.Add(1)
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repair did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("policy owner could retire rules while repair was still running")
	default:
	}
	finishRepair()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("policy repair did not retire after cancellation")
	}
	if calls.Load() != 1 || reports.Load() != 1 {
		t.Fatalf("canceled repair repeated or lost its error: %d calls, %d reports", calls.Load(), reports.Load())
	}
}
