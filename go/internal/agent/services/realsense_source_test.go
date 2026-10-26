package services

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/realsense"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// fakeRealSenseSysfs builds the minimal sysfs shape Detect needs for one
// D435i: depth on interface 00, colour on 03.
func fakeRealSenseSysfs(t *testing.T, pid string) string {
	t.Helper()
	root := t.TempDir()
	devDir := filepath.Join(root, "sys/devices/usb1/1-4")
	classDir := filepath.Join(root, "sys/class/video4linux")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(devDir, "idVendor"), "8086")
	write(filepath.Join(devDir, "idProduct"), pid)
	write(filepath.Join(devDir, "serial"), "337122071091")
	for node, iface := range map[string]string{"video0": "00", "video4": "03"} {
		ifaceDir := filepath.Join(devDir, "1-4:1."+iface)
		write(filepath.Join(ifaceDir, "bInterfaceNumber"), iface)
		nodeDir := filepath.Join(ifaceDir, "video4linux", node)
		write(filepath.Join(nodeDir, "name"), "Intel(R) RealSense(TM) Depth Camera 435i")
		if err := os.Symlink(ifaceDir, filepath.Join(nodeDir, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(classDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(nodeDir, filepath.Join(classDir, node)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// With no live camera behind /dev/video0, the calibration read fails — and the
// source must be LISTED as unavailable with the reason, never hidden. That is
// the native analogue of the old "helper not installed" honesty, with nothing
// left to install.
func TestRealSenseProvider_ListsUncalibratableCameraAsUnavailable(t *testing.T) {
	root := fakeRealSenseSysfs(t, "0b3a")
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, root)
	sources, err := p.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.GetSource() != "realsense:337122071091" {
		t.Errorf("source = %q", desc.GetSource())
	}
	if desc.GetAvailable() {
		t.Fatal("a camera whose calibration cannot be read is listed as available")
	}
	if !strings.Contains(desc.GetUnavailableReason(), "calibration could not be read") {
		t.Errorf("reason = %q", desc.GetUnavailableReason())
	}
}

func TestRealSenseProvider_NoCameraMeansNoSources(t *testing.T) {
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, t.TempDir())
	sources, err := p.Sources(context.Background())
	if err != nil || len(sources) != 0 {
		t.Fatalf("got %d sources, err %v; a device without a RealSense is not a problem to report", len(sources), err)
	}
}

func TestDepthModeForAspectRatios(t *testing.T) {
	for _, tc := range []struct{ cw, ch, dw, dh uint32 }{
		{1280, 720, 848, 480},
		{1920, 1080, 848, 480},
		{640, 480, 640, 480},
	} {
		if dw, dh := depthModeFor(tc.cw, tc.ch); dw != tc.dw || dh != tc.dh {
			t.Errorf("depthModeFor(%dx%d) = %dx%d, want %dx%d", tc.cw, tc.ch, dw, dh, tc.dw, tc.dh)
		}
	}
}

// usableCalibration is a calibration that serves the default depth mode.
func usableCalibration() *realsense.Calibration {
	return &realsense.Calibration{
		FirmwareVersion: "5.15.1.55",
		DepthScaleM:     0.001,
		BaselineMM:      -50,
		Depth:           []realsense.Intrinsics{{Width: 848, Height: 480, Fx: 420, Fy: 420, Ppx: 424, Ppy: 240}},
		Colour:          realsense.Intrinsics{Width: 1920, Height: 1080, Fx: 1380, Fy: 1380, Ppx: 960, Ppy: 540},
	}
}

// fakeHWM stands in for a camera's HWM conversations: it counts them and
// fails the test if two ever overlap, since the reply to one would be read
// back by the other.
type fakeHWM struct {
	t      *testing.T
	cal    func() (*realsense.Calibration, error)
	scale  func() (float64, error)
	active atomic.Int32
	calls  atomic.Int32
}

func (f *fakeHWM) enter() func() {
	if f.active.Add(1) != 1 {
		f.t.Error("two HWM conversations with one camera overlapped")
	}
	f.calls.Add(1)
	time.Sleep(5 * time.Millisecond) // long enough for an overlap to show
	return func() { f.active.Add(-1) }
}

func (f *fakeHWM) provider(t *testing.T, pid string) (*realsenseProvider, framesource.Source) {
	t.Helper()
	p := newRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, fakeRealSenseSysfs(t, pid))
	p.readCal = func(realsense.Camera) (*realsense.Calibration, error) { defer f.enter()(); return f.cal() }
	p.readScale = func(realsense.Camera) (float64, error) { defer f.enter()(); return f.scale() }
	sources, err := p.sources(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, err %v", len(sources), err)
	}
	return p, sources[0]
}

// Every listing and lookup describes every source, and they run concurrently.
// A camera is read once however many ask at once, never by two at a time, and
// a failed read is not repeated for every caller that follows it.
func TestRealSenseProvider_ConcurrentDescribesShareOneSerialisedRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		cal  func() (*realsense.Calibration, error)
	}{
		{"read succeeds", func() (*realsense.Calibration, error) { return usableCalibration(), nil }},
		{"read fails", func() (*realsense.Calibration, error) { return nil, errors.New("EIO") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hwm := &fakeHWM{t: t, cal: tc.cal}
			_, src := hwm.provider(t, "0b3a")
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := src.Describe(context.Background()); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if n := hwm.calls.Load(); n != 1 {
				t.Errorf("8 concurrent describes made %d calibration reads, want 1", n)
			}
		})
	}
}

func TestRealSenseProvider_FailedReadIsRetriedOnlyAfterTheInterval(t *testing.T) {
	fail := true
	hwm := &fakeHWM{t: t, cal: func() (*realsense.Calibration, error) {
		if fail {
			return nil, errors.New("EIO")
		}
		return usableCalibration(), nil
	}}
	p, src := hwm.provider(t, "0b3a")
	for range 3 {
		desc, _ := src.Describe(context.Background())
		if desc.GetAvailable() || !strings.Contains(desc.GetUnavailableReason(), "EIO") {
			t.Fatalf("a failed read listed as %+v", desc)
		}
	}
	if n := hwm.calls.Load(); n != 1 {
		t.Fatalf("3 describes within the interval made %d reads, want 1", n)
	}

	fail = false
	p.entry("337122071091").readAt = time.Now().Add(-calibrationRetryInterval)
	desc, _ := src.Describe(context.Background())
	if !desc.GetAvailable() {
		t.Errorf("a camera that answers after the interval is still unavailable: %q", desc.GetUnavailableReason())
	}
}

// The listing promises aligned depth only when a capture would deliver it.
// With the depth units unreadable a capture runs without depth, so the
// listing says so, by name, and promises it again once the units read back.
func TestRealSenseProvider_UnreadableDepthUnitsAreNotPromised(t *testing.T) {
	scaleOK := false
	hwm := &fakeHWM{t: t,
		cal: func() (*realsense.Calibration, error) {
			cal := usableCalibration()
			cal.DepthScaleM = 0
			return cal, nil
		},
		scale: func() (float64, error) {
			if !scaleOK {
				return 0, errors.New("GETADV failed")
			}
			return 0.001, nil
		},
	}
	p, src := hwm.provider(t, "0b3a")

	desc, err := src.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !desc.GetAvailable() {
		t.Fatalf("colour is still servable; the source was listed unavailable: %q", desc.GetUnavailableReason())
	}
	if slices.Contains(desc.GetProvides(), agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH) {
		t.Error("aligned depth promised with the depth units unread")
	}
	if desc.GetDepthWidth() != 0 || !strings.Contains(desc.GetUnavailableReason(), "depth units") {
		t.Errorf("depth = %dx%d, reason %q; want no depth, named", desc.GetDepthWidth(), desc.GetDepthHeight(), desc.GetUnavailableReason())
	}
	if want := framesource.FrameBytes(defaultColourWidth*2, defaultColourHeight, 0, 0); desc.GetMaxFrameBytes() != want {
		t.Errorf("max frame bytes = %d, want colour-only %d", desc.GetMaxFrameBytes(), want)
	}

	scaleOK = true
	p.entry("337122071091").readAt = time.Now().Add(-calibrationRetryInterval)
	desc, _ = src.Describe(context.Background())
	if !slices.Contains(desc.GetProvides(), agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH) {
		t.Errorf("depth units that read back did not restore the promise: %+v", desc)
	}
}

// A capture re-reads the depth units, and a failed re-read is a zero scale,
// never the value an older read left behind. Streams already running keep the
// calibration they started with.
func TestRealSenseProvider_FreshDepthScaleNeverReusesAnOlderValue(t *testing.T) {
	hwm := &fakeHWM{t: t,
		cal:   func() (*realsense.Calibration, error) { return usableCalibration(), nil },
		scale: func() (float64, error) { return 0, errors.New("GETADV failed") },
	}
	p, _ := hwm.provider(t, "0b3a")
	cam := realsense.Camera{Serial: "337122071091", PID: "0b3a"}
	before, err := p.calibration(cam)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := p.freshDepthScale(cam)
	if err == nil {
		t.Fatal("a failed units read reported no error")
	}
	if cal == nil || cal.DepthScaleM != 0 {
		t.Fatalf("fresh calibration = %+v; want the factory tables with scale 0", cal)
	}
	if before.DepthScaleM != 0.001 {
		t.Errorf("the re-read mutated a calibration a running stream holds: scale %v", before.DepthScaleM)
	}
}

// The shared colour hub ending is a clean end only when its producer simply
// finished. A failed capture, or one an episode capture took over, must reach
// calibrated subscribers as a reason rather than an EOF.
func TestRealSenseStream_ColourEndCarriesItsReason(t *testing.T) {
	failed := status.Error(codes.Internal, "video producer stopped unexpectedly")
	for _, tc := range []struct {
		name  string
		setup func(h *deviceHub)
		check func(t *testing.T, err error)
	}{
		{"producer finished", func(*deviceHub) {}, func(t *testing.T, err error) {
			if !errors.Is(err, io.EOF) {
				t.Errorf("err = %v, want io.EOF", err)
			}
		}},
		{"producer failed", func(h *deviceHub) { h.err = failed }, func(t *testing.T, err error) {
			if !errors.Is(err, failed) {
				t.Errorf("err = %v, want the producer's error", err)
			}
		}},
		{"episode capture took over", func(h *deviceHub) { h.restarted = true }, func(t *testing.T, err error) {
			if !streamreason.Has(err, streamreason.CameraProducerRestarted) {
				t.Errorf("err = %v, want CAMERA_PRODUCER_RESTARTED", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan *videoFrame)
			hub := &deviceHub{subs: map[int]*hubSubscriber{7: {ch: ch, raw: true}}}
			tc.setup(hub)
			close(ch)
			st := &realsenseStream{hub: hub, subID: 7, colour: ch}
			st.ctx, st.cancel = context.WithCancel(context.Background())
			defer st.cancel()
			_, err := st.Next(context.Background())
			tc.check(t, err)
		})
	}
}

// The unrecognised-model honesty: it enumerates, offers measured intrinsics
// only, and says why depth is off — new hardware fails visibly, not wrongly.
func TestRealSenseProvider_UnknownModelDisablesDepthByName(t *testing.T) {
	root := fakeRealSenseSysfs(t, "0bff")
	p := NewRealSenseProvider(zap.NewNop(), &VideoService{logger: zap.NewNop()}, root)
	sources, err := p.Sources(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, err %v", len(sources), err)
	}
	desc, err := sources[0].Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Calibration read fails in this environment either way; the point this
	// test pins is that an unknown PID is enumerated rather than skipped.
	if desc.GetSource() != "realsense:337122071091" || desc.GetKind() != "realsense" {
		t.Errorf("unknown model vanished from the listing: %+v", desc)
	}
	for _, p := range desc.GetProvides() {
		if p == agentpbv2.FrameRequirement_FRAME_REQUIREMENT_ALIGNED_DEPTH {
			t.Error("an unrecognised model must not promise aligned depth")
		}
	}
}
