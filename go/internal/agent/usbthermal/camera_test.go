package usbthermal

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed before a frame arrived")
		}
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no frame within 5s")
	}
	panic("unreachable")
}

func waitClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscription not closed within 5s")
		}
	}
}

// TestCameraServesRawFramesAndPictures runs the whole driver against the fake
// device replaying the real stream: the start sequence, reassembly, delivery
// to both kinds of subscriber, and the stop when the last one leaves.
func TestCameraServesRawFramesAndPictures(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	dev.bulk = replay(t, fixtureCounter)
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)

	rawCtx, stopRaw := context.WithCancel(context.Background())
	raw, err := m.SubscribeRaw(rawCtx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	picCtx, stopPic := context.WithCancel(context.Background())
	pics, err := m.SubscribePictures(picCtx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}

	block := realBlock(t)
	f := receive(t, raw)
	if f.Width != 256 || f.Height != 384 || f.BytesPerLine != 512 || len(f.Data) != 196608 {
		t.Fatalf("raw frame %dx%d, %d bytes/line, %d bytes; want 256x384, 512, 196608", f.Width, f.Height, f.BytesPerLine, len(f.Data))
	}
	// The counter is not part of the first 384 rows, so every replayed frame
	// carries the fixture's picture and temperatures exactly.
	if !bytes.Equal(f.Data, block[:196608]) {
		t.Fatal("raw frame is not the block's picture and temperature rows")
	}
	if f.Sequence < fixtureCounter || f.Timestamp.IsZero() {
		t.Fatalf("raw frame sequence %d, timestamp %v", f.Sequence, f.Timestamp)
	}
	if c, ok := f.Temperature(0, 6); !ok || CountsToCelsius(c) < 28 || CountsToCelsius(c) > 28.2 {
		t.Fatalf("hottest pixel reads %v degC", CountsToCelsius(c))
	}

	p := receive(t, pics)
	if p.Width != 256 || p.Height != 192 || p.Format != FormatYUY2 || len(p.Data) != 98304 {
		t.Fatalf("picture %dx%d %s, %d bytes; want 256x192 YUY2, 98304", p.Width, p.Height, p.Format, len(p.Data))
	}
	if !bytes.Equal(p.Data, block[:98304]) || p.Timestamp.IsZero() {
		t.Fatal("picture is not the block's picture rows")
	}
	if cap(p.Data) != len(p.Data) || cap(f.Data) != len(f.Data) {
		t.Fatal("frames must not expose the bytes past their end to append")
	}

	// The last subscriber leaving stops the camera and releases it.
	stopRaw()
	waitClosed(t, raw)
	if dev.isClosed() {
		t.Fatal("the camera stopped while a picture subscriber remained")
	}
	stopPic()
	waitClosed(t, pics)
	waitFor(t, "the camera to be released", dev.isClosed)
	log := dev.callLog()
	if got := log[len(log)-4:]; !slices.Equal(got, []string{"setintf 1 0", "release 1", "release 0", "close"}) {
		t.Fatalf("the camera was left %q", got)
	}
	if err := m.LastError(exxonStableID); err != nil {
		t.Fatalf("LastError = %v after a clean stop", err)
	}
}

// TestCameraReconnectsAfterUnplug: the camera disappears mid-stream and comes
// back on a new device number; the subscriber keeps its channel and frames
// resume.
func TestCameraReconnectsAfterUnplug(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	first := newFakeDevice()
	src := replay(t, 100)
	var mu sync.Mutex
	reads := 0
	first.bulk = func(buf []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if reads++; reads > 40 { // about three frames, then the cable is pulled
			return 0, errors.New("no such device")
		}
		return src(buf)
	}
	b.attach(b.node(1, 15), first)
	second := newFakeDevice()
	second.bulk = replay(t, 500)
	b.attach(b.node(1, 16), second)
	m := newTestManager(t, b, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := m.SubscribeRaw(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, frames)
	waitFor(t, "the unplug to be noticed", func() bool { return m.LastError(exxonStableID) != nil })
	if err := m.LastError(exxonStableID); !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("LastError = %v", err)
	}
	if !first.isClosed() {
		t.Fatal("the unplugged device was not closed")
	}
	// Replugged: a new device number, as the kernel assigns on every plug.
	b.unplug("1-2.3")
	b.plugTC001Max("1-2.3", 16)
	for {
		if f := receive(t, frames); f.Sequence >= 500 {
			break
		}
	}
	if err := m.LastError(exxonStableID); err != nil {
		t.Fatalf("LastError = %v while frames flow again", err)
	}
	opened := b.openedPaths()
	if opened[0] != b.node(1, 15) || opened[len(opened)-1] != b.node(1, 16) {
		t.Fatalf("opened %q; want the old node first and the new one last", opened)
	}
}

// TestCameraHeldByAnotherProcess: a refused claim is reported, retried, and
// leaves the camera as it was found.
func TestCameraHeldByAnotherProcess(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	dev.claimErr = ErrBusy
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	frames, err := m.SubscribeRaw(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the refusal to be reported", func() bool { return m.LastError(exxonStableID) != nil })
	err = m.LastError(exxonStableID)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("LastError = %v", err)
	}
	waitFor(t, "a retry", func() bool { return len(b.openedPaths()) >= 2 })
	cancel()
	waitClosed(t, frames)
	for _, c := range dev.callLog() {
		if strings.HasPrefix(c, "setintf") || strings.HasPrefix(c, "cmd") {
			t.Fatalf("touched the camera (%s) without owning its interfaces", c)
		}
	}
}

// TestCameraRestartWaitsForTheLastSession: leaving and rejoining at once must
// not open the camera twice; the new session starts after the old one has
// released it.
func TestCameraRestartWaitsForTheLastSession(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	dev.bulk = replay(t, 1)
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	for range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		frames, err := m.SubscribeRaw(ctx, exxonStableID)
		if err != nil {
			t.Fatal(err)
		}
		receive(t, frames)
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := m.SubscribeRaw(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, frames)
	b.mu.Lock()
	maxOpen := b.maxOpen
	b.mu.Unlock()
	if maxOpen != 1 {
		t.Fatalf("the camera was open %d times at once", maxOpen)
	}
}

// TestSlowSubscriberDropsItsOwnFrames: a subscriber that never reads loses
// frames; nobody else does, and capture never waits for it.
func TestSlowSubscriberDropsItsOwnFrames(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	dev.bulk = replay(t, 1)
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := m.SubscribeRaw(ctx, exxonStableID); err != nil { // never read
		t.Fatal(err)
	}
	pics, err := m.SubscribePictures(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 * subscriberBuffer * 3 {
		receive(t, pics)
	}
}

// TestManagerClose ends every subscription, leaves each camera released, and
// refuses new ones.
func TestManagerClose(t *testing.T) {
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	dev.bulk = replay(t, 1)
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	frames, err := m.SubscribeRaw(context.Background(), exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, frames)
	m.Close()
	waitClosed(t, frames)
	if !dev.isClosed() {
		t.Fatal("Close returned before the camera was released")
	}
	if _, err := m.SubscribeRaw(context.Background(), exxonStableID); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after Close: %v, want ErrClosed", err)
	}
}

// TestSubscribeToAMissingCamera is an error, not a silent wait.
func TestSubscribeToAMissingCamera(t *testing.T) {
	m := newTestManager(t, newFakeBus(t), nil)
	if _, err := m.SubscribePictures(context.Background(), exxonStableID); err == nil {
		t.Fatal("subscribed to a camera that is not connected")
	}
}

func TestNoFramesErrorSaysWhy(t *testing.T) {
	if err := noFramesError(Stats{Bytes: 1 << 20, Rejected: 3}); !strings.Contains(err.Error(), "failed validation") {
		t.Errorf("rejected blocks: %v", err)
	}
	if err := noFramesError(Stats{Bytes: 1 << 20}); !strings.Contains(err.Error(), "none of them formed a frame") {
		t.Errorf("no blocks: %v", err)
	}
}
