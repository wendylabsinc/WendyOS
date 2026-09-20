// Package framesourcetest is a calibrated frame source driven entirely from a
// test: no camera, no helper process, no kernel.
//
// It exists because every property this feature promises — that a requirement
// is refused by name, that losing depth mid-stream ends the stream, that a slow
// subscriber never stalls a fast one — has to be provable in CI on a machine
// with no RGB-D camera attached. A property only checkable by plugging in a
// D435i is a property that regresses unnoticed, which is the class of failure
// this whole feature exists to remove.
package framesourcetest

import (
	"context"
	"io"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Fake is a framesource.Source whose frames a test pushes by hand.
type Fake struct {
	mu     sync.Mutex
	desc   *agentpbv2.CalibratedSource
	frames chan *agentpbv2.CalibratedFrame
	endErr error
	opens  int
	closes int
	// OpenErr, when set, makes Open fail.
	OpenErr error
	// LastOptions records the geometry the service asked for.
	LastOptions framesource.Options
}

// NewFake builds a source advertising exactly the given requirements at the
// given colour geometry. Depth geometry mirrors colour, since an aligned source
// has no other option.
func NewFake(name string, width, height uint32, provides ...agentpbv2.FrameRequirement) *Fake {
	desc := &agentpbv2.CalibratedSource{
		Source:       name,
		Kind:         "fake",
		Description:  "test source",
		Provides:     provides,
		ColourWidth:  width,
		ColourHeight: height,
		ColourFourcc: "BGR3",
		Available:    true,
	}
	for _, r := range provides {
		if r == agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH {
			desc.DepthWidth, desc.DepthHeight = width, height
		}
	}
	desc.MaxFrameBytes = framesource.FrameBytes(width*3, height, desc.GetDepthWidth()*2, desc.GetDepthHeight())
	return &Fake{desc: desc, frames: make(chan *agentpbv2.CalibratedFrame, 64)}
}

// Desc exposes the descriptor so a test can adjust it before listing.
func (f *Fake) Desc() *agentpbv2.CalibratedSource { return f.desc }

func (f *Fake) Describe(context.Context) (*agentpbv2.CalibratedSource, error) { return f.desc, nil }

func (f *Fake) Open(_ context.Context, opts framesource.Options) (framesource.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OpenErr != nil {
		return nil, f.OpenErr
	}
	f.opens++
	f.LastOptions = opts
	return &fakeStream{f: f}, nil
}

// Push queues a frame for the next Next call.
func (f *Fake) Push(frame *agentpbv2.CalibratedFrame) { f.frames <- frame }

// End makes the stream finish with err (nil for a clean end) once the queued
// frames have been read.
func (f *Fake) End(err error) {
	f.mu.Lock()
	f.endErr = err
	f.mu.Unlock()
	close(f.frames)
}

// Opens reports how many times the service opened this source — the check that
// several subscribers share ONE capture rather than each taking the camera.
func (f *Fake) Opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// Closes reports how many opened streams were closed again.
func (f *Fake) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

type fakeStream struct {
	f         *Fake
	closeOnce sync.Once
}

func (s *fakeStream) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-s.f.frames:
		if !ok {
			s.f.mu.Lock()
			err := s.f.endErr
			s.f.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		return frame, nil
	}
}

// Close is deliberately once-only and safe to call concurrently with Next: the
// service closes a stream from its cancellation watchdog as well as on return,
// which is exactly what the Stream contract requires of a real source.
func (s *fakeStream) Close() error {
	s.closeOnce.Do(func() {
		s.f.mu.Lock()
		s.f.closes++
		s.f.mu.Unlock()
	})
	return nil
}

// --- frame builders ---

// Frame builds a complete, valid calibrated frame: BGR colour, aligned depth at
// the same resolution with a millimetre scale, measured intrinsics and applied
// capture settings. Tests take pieces off it to describe a degraded source.
func Frame(id uint64, width, height uint32) *agentpbv2.CalibratedFrame {
	colourStride := width * 3
	depthStride := width * 2
	return &agentpbv2.CalibratedFrame{
		FrameId:      id,
		CapturedAtNs: 1_000_000_000 + id,
		Colour:       make([]byte, int(colourStride)*int(height)),
		ColourFormat: &agentpb.RawFormat{
			Width: width, Height: height, Fourcc: "BGR3", BytesPerLine: colourStride,
		},
		Depth: &agentpbv2.DepthPlane{
			Data:         make([]byte, int(depthStride)*int(height)),
			Width:        width,
			Height:       height,
			BytesPerLine: depthStride,
			ScaleM:       0.001,
			Alignment:    agentpbv2.DepthPlane_ALIGNMENT_ALIGNED_TO_COLOUR,
		},
		Intrinsics: &agentpbv2.CameraIntrinsics{
			Fx: 640, Fy: 640, Cx: float32(width) / 2, Cy: float32(height) / 2,
			Width: width, Height: height,
			Provenance: agentpbv2.CameraIntrinsics_PROVENANCE_MEASURED,
			Note:       "fake factory calibration",
		},
		Settings: &agentpbv2.CaptureSettings{
			ExposureRaw: 156, ExposureUs: 15600, ExposureUnitUs: 100, Gain: 64,
		},
	}
}

// ColourOnly is Frame with no depth plane — a colour camera, or a depth sensor
// that dropped out.
func ColourOnly(id uint64, width, height uint32) *agentpbv2.CalibratedFrame {
	f := Frame(id, width, height)
	f.Depth = nil
	return f
}
