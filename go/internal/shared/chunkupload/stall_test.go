package chunkupload

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWatchCancelsAStalledUpload is the #1765 shape: streams are open and
// nothing moves. The watchdog must cancel the upload with ErrStalled.
func TestWatchCancelsAStalledUpload(t *testing.T) {
	src, refs := layerFixture(t, 64, 300)
	f := newFakeClient()
	f.stallAfter = 10
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a := &Activity{}
	stop := Watch(a, 200*time.Millisecond, cancel)
	defer stop()

	start := time.Now()
	err := Upload(ctx, f, src, refs, Options{BatchChunks: 8, Streams: 2, Activity: a})
	if err == nil {
		t.Fatal("a stalled upload returned nil")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrStalled) {
		t.Fatalf("cause = %v, want ErrStalled", cause)
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 3*time.Second {
		t.Fatalf("stall detected after %v, want about the 200ms timeout", took)
	}
}

// TestWatchLeavesASlowUploadAlone: every Send is slow, but some stream makes
// progress well within each timeout window, so the watchdog must not fire.
func TestWatchLeavesASlowUploadAlone(t *testing.T) {
	src, refs := layerFixture(t, 40, 300)
	f := newFakeClient()
	f.sendDelay = 20 * time.Millisecond
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a := &Activity{}
	stop := Watch(a, 150*time.Millisecond, cancel)
	defer stop()

	if err := Upload(ctx, f, src, refs, Options{BatchChunks: 8, Streams: 2, Activity: a}); err != nil {
		t.Fatalf("slow upload failed: %v", err)
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("a slow but moving upload was cancelled: %v", cause)
	}
}

// TestWatchIgnoresTimeWithNoStreamOpen: local work between streams (lazy
// decompression, QueryChunks) is not a stall; an open stream that stops is.
func TestWatchIgnoresTimeWithNoStreamOpen(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a := &Activity{}
	stop := Watch(a, 50*time.Millisecond, cancel)
	defer stop()

	time.Sleep(250 * time.Millisecond)
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("watchdog fired with no stream open: %v", cause)
	}
	a.opened()
	deadline := time.Now().Add(2 * time.Second)
	for context.Cause(ctx) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cause := context.Cause(ctx); !errors.Is(cause, ErrStalled) {
		t.Fatalf("cause = %v, want ErrStalled once a stream sat open with no progress", cause)
	}
}

// TestNilActivityRecordsNothing: callers that pass no Activity (the build
// host, gzip uploads) must work exactly as before.
func TestNilActivityRecordsNothing(t *testing.T) {
	var a *Activity
	a.opened()
	a.progressed()
	a.closed()
	src, refs := layerFixture(t, 20, 300)
	if err := Upload(context.Background(), newFakeClient(), src, refs, Options{BatchChunks: 8}); err != nil {
		t.Fatal(err)
	}
}
