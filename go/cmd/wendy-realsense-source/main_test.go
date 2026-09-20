package main

// The helper's own behaviour — argument handling, record framing, shutdown —
// proved against a capturer a test drives. Only the librealsense binding needs
// a camera; everything around it is checked here, on any machine.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type fakeCapturer struct {
	sources  []*agentpbv2.CalibratedSource
	frames   []*agentpbv2.CalibratedFrame
	lastOpts CaptureOptions
	openErr  error
	closed   bool
	// opened is what Open handed back, so a test can check the capture was
	// released. Set to a capture of its own to override what Open returns.
	opened capture
}

func (c *fakeCapturer) Describe(context.Context) ([]*agentpbv2.CalibratedSource, error) {
	return c.sources, nil
}

func (c *fakeCapturer) Open(_ context.Context, opts CaptureOptions) (capture, error) {
	c.lastOpts = opts
	if c.openErr != nil {
		return nil, c.openErr
	}
	if c.opened == nil {
		c.opened = &fakeCapture{
			desc:   &agentpbv2.CalibratedSource{Source: opts.Source, ColourWidth: opts.Width, ColourHeight: opts.Height},
			frames: c.frames,
		}
	}
	return c.opened, nil
}

func (c *fakeCapturer) Close() error { c.closed = true; return nil }

type fakeCapture struct {
	desc   *agentpbv2.CalibratedSource
	frames []*agentpbv2.CalibratedFrame
	i      int
	closed bool
}

func (c *fakeCapture) Descriptor() *agentpbv2.CalibratedSource { return c.desc }

func (c *fakeCapture) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.i >= len(c.frames) {
		return nil, io.EOF
	}
	f := c.frames[c.i]
	c.i++
	return f, nil
}

func (c *fakeCapture) Close() error { c.closed = true; return nil }

// holdParentPipe replaces the stdin watchdog with a pipe that stays open, so a
// test's own /dev/null stdin does not end the capture on the first read.
func holdParentPipe(t *testing.T) {
	t.Helper()
	pr, pw := io.Pipe()
	prev := parentPipe
	parentPipe = pr
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
		parentPipe = prev
	})
}

func TestRun_DescribeWritesOneRecordPerCamera(t *testing.T) {
	cc := &fakeCapturer{sources: []*agentpbv2.CalibratedSource{
		{Source: "realsense:111", Kind: "realsense"},
		{Source: "realsense:222", Kind: "realsense"},
	}}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"describe"}, &out,
		func() (capturer, error) { return cc, nil }); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"realsense:111", "realsense:222"} {
		src, err := framesource.ReadSource(&out)
		if err != nil {
			t.Fatalf("reading %s: %v", want, err)
		}
		if src.GetSource() != want {
			t.Errorf("source = %q, want %q", src.GetSource(), want)
		}
	}
	if _, _, err := framesource.ReadRecord(&out); !errors.Is(err, io.EOF) {
		t.Errorf("trailing data after the last descriptor: %v", err)
	}
	if !cc.closed {
		t.Error("the SDK context was not released")
	}
}

// The descriptor is written BEFORE any frame so the agent can check a
// subscriber's requirements against what was actually negotiated, not against
// what a listing promised a moment ago.
func TestRun_StreamWritesTheNegotiatedDescriptorBeforeAnyFrame(t *testing.T) {
	holdParentPipe(t)
	cc := &fakeCapturer{frames: []*agentpbv2.CalibratedFrame{{FrameId: 1}, {FrameId: 2}}}
	var out bytes.Buffer
	err := run(context.Background(),
		[]string{"stream", "--source", "realsense:111", "--width", "640", "--height", "480", "--fps", "15"},
		&out, func() (capturer, error) { return cc, nil })
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if cc.lastOpts != (CaptureOptions{Source: "realsense:111", Width: 640, Height: 480, Framerate: 15}) {
		t.Errorf("capture options = %+v", cc.lastOpts)
	}
	src, err := framesource.ReadSource(&out)
	if err != nil {
		t.Fatalf("first record is not a descriptor: %v", err)
	}
	if src.GetSource() != "realsense:111" || src.GetColourWidth() != 640 {
		t.Errorf("descriptor = %+v", src)
	}
	for i := uint64(1); i <= 2; i++ {
		f, err := framesource.ReadFrame(&out)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.GetFrameId() != i {
			t.Errorf("frame id = %d, want %d", f.GetFrameId(), i)
		}
	}
	if !cc.opened.(*fakeCapture).closed {
		t.Error("the capture was not closed, so the camera would stay held")
	}
}

// The parent going away must release the camera, or an orphan holds it for as
// long as the machine is up.
func TestRun_StreamEndsWhenTheParentPipeCloses(t *testing.T) {
	pr, pw := io.Pipe()
	prev := parentPipe
	parentPipe = pr
	t.Cleanup(func() { parentPipe = prev })

	blocking := &blockingCapture{desc: &agentpbv2.CalibratedSource{Source: "realsense:111"}}
	cc := &fakeCapturer{opened: blocking}
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), []string{"stream"}, io.Discard,
			func() (capturer, error) { return cc, nil })
	}()

	_ = pw.Close() // the agent died
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run ended with %v, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper kept the camera after its parent went away")
	}
	if !blocking.closed {
		t.Error("the capture was not released when the parent went away")
	}
}

type blockingCapture struct {
	desc   *agentpbv2.CalibratedSource
	closed bool
}

func (c *blockingCapture) Descriptor() *agentpbv2.CalibratedSource { return c.desc }

func (c *blockingCapture) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *blockingCapture) Close() error { c.closed = true; return nil }

func TestRun_UnknownCommandPrintsTheUsage(t *testing.T) {
	err := run(context.Background(), []string{"capture"}, io.Discard,
		func() (capturer, error) { return &fakeCapturer{}, nil })
	if err == nil || !strings.Contains(err.Error(), "describe") {
		t.Errorf("err = %v; an unknown command must show what the helper can do", err)
	}
}

func TestRun_NoArgumentsPrintsTheUsage(t *testing.T) {
	err := run(context.Background(), nil, io.Discard,
		func() (capturer, error) { return &fakeCapturer{}, nil })
	if err == nil || !strings.Contains(err.Error(), "stream") {
		t.Errorf("err = %v", err)
	}
}

// A helper that started, produced nothing and exited 0 would be exactly the
// silent nothing this whole feature exists to remove. The build without
// librealsense refuses, and says how to build one that works.
func TestNewCapturer_WithoutLibrealsenseRefusesAndSaysHowToBuildIt(t *testing.T) {
	_, err := newCapturer()
	if err == nil {
		t.Skip("this binary was built with -tags realsense; the unsupported path is not compiled in")
	}
	for _, want := range []string{"librealsense", "-tags realsense"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
