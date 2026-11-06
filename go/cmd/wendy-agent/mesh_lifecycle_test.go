package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMeshLifetimeStopWaitsForResetCleanup(t *testing.T) {
	var lifetime meshLifetime
	canceled, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	lifetime.Start(context.Background(), func(ctx context.Context, launch func(func())) {
		launch(func() { <-ctx.Done(); close(canceled); <-release })
	})
	go func() { _ = lifetime.Stop(context.Background()); close(stopped) }()
	<-canceled
	select {
	case <-stopped:
		t.Fatal("reset returned before the mesh worker drained")
	default:
	}
	close(release)
	<-stopped
	lifetime.Start(context.Background(), func(context.Context, func(func())) { t.Error("reset restarted the old identity") })
	_ = lifetime.Stop(context.Background())
}

func TestMeshLifetimeParentShutdownDrainsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var lifetime meshLifetime
	var drained atomic.Int32
	lifetime.Start(ctx, func(ctx context.Context, launch func(func())) {
		for range 4 {
			launch(func() { <-ctx.Done(); drained.Add(1) })
		}
	})
	cancel()
	_ = lifetime.Stop(context.Background())
	if drained.Load() != 4 {
		t.Fatal("shutdown returned before every enrolled worker drained")
	}
}

func TestMeshLifetimeEnrollmentResetRace(t *testing.T) {
	for range 100 {
		var lifetime meshLifetime
		var work sync.WaitGroup
		work.Add(2)
		go func() {
			defer work.Done()
			lifetime.Start(context.Background(), func(ctx context.Context, launch func(func())) {
				launch(func() { <-ctx.Done() })
			})
		}()
		go func() { defer work.Done(); _ = lifetime.Stop(context.Background()) }()
		work.Wait()
	}
}

func TestMeshLifetimeStopDeadlineKeepsResetUnconfirmed(t *testing.T) {
	var lifetime meshLifetime
	release := make(chan struct{})
	lifetime.Start(context.Background(), func(_ context.Context, launch func(func())) {
		launch(func() { <-release })
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := lifetime.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck mesh cleanup was reported complete: %v", err)
	}
	close(release)
	if err := lifetime.Stop(context.Background()); err != nil {
		t.Fatalf("shutdown did not finish after the worker drained: %v", err)
	}
}

func TestResetMeshRecoveryRetriesBeforeReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("owned rule removal failed")
	var attempts atomic.Int32
	states := make(chan error, 4)
	done := recoverCompletedResetMesh(ctx, true, time.Millisecond, func(context.Context) error {
		if attempts.Add(1) == 1 {
			return failure
		}
		return nil
	}, func(err error) { states <- err }, func(error) {})
	if err := <-states; !errors.Is(err, failure) {
		t.Fatalf("startup did not hide reset readiness synchronously: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset cleanup was not retried")
	}
	if err := <-states; err != nil || attempts.Load() != 2 {
		t.Fatalf("successful cleanup did not restore readiness: %v, attempts %d", err, attempts.Load())
	}
}

func TestResetMeshRecoverySkipsHostsWithoutVerifiedCompletion(t *testing.T) {
	<-recoverCompletedResetMesh(context.Background(), false, time.Millisecond, func(context.Context) error {
		t.Fatal("host without a completed reset attempted mesh cleanup or dependency checks")
		return nil
	}, func(error) { t.Fatal("fresh enrollment readiness changed") }, func(error) {})
}
