package services

// USB thermal cameras the agent drives from userspace (go/internal/agent/usbthermal).
//
// The TOPDON TC001 Max has no UVC interface and no kernel driver, so it never
// becomes a /dev/videoN node: none of the V4L2 or GStreamer paths in
// video_service.go can open it. The usbthermal package owns the device through
// usbfs; this file is the thin layer that puts it in front of clients:
//
//   - ListVideoDevices and RefreshCameras list it, with an id from
//     usbthermal's band (88-95) and a serial-based stable id;
//   - StreamVideo serves its raw thermal frames (VIDEO_CODEC_RAW) through an
//     ordinary hub, so subscriber accounting, drop handling and teardown are
//     the same as for every other camera, and the hub's last subscriber leaving
//     is what stops the camera.
//
// Encoded video (the default codec) is the in-process live view's: the camera
// is registered as an in-process camera under the same id, fed from
// usbthermal's picture frames (video_usbthermal_live.go), and StreamVideo
// sends an encoded request there before it resolves the id. Only a raw request
// reaches streamUSBThermal, unless the live view could not register the camera.
//
// Deliberately NOT in listCameras: that listing also feeds episode capture
// (data_camera_adapter.go) and the two-plane loopback path, and both address a
// camera by its /dev/video node. Until they can take frames from an in-process
// source, this camera appears in the RPC listings only.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbthermal"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// usbThermalHubKeyPrefix marks a hub key as a userspace USB thermal camera;
// the stable id follows.
const usbThermalHubKeyPrefix = "usbthermal:"

// Vars rather than constants only so tests can shorten them.
var (
	// usbThermalFirstFrameTimeout bounds the wait for a stream's first frame.
	// The start sequence alone takes about four seconds, and the driver retries
	// a failed start with backoff, so this allows a couple of attempts.
	usbThermalFirstFrameTimeout = 20 * time.Second
	// usbThermalStallTimeout ends a stream whose frames stopped: the camera was
	// unplugged and has not come back, or no longer streams.
	usbThermalStallTimeout = 10 * time.Second
)

// usbThermalDriver is what the video service needs from the userspace USB
// thermal driver. *usbthermal.Manager implements it; tests substitute a fake.
type usbThermalDriver interface {
	List() []usbthermal.Listing
	SubscribeRaw(ctx context.Context, stableID string) (<-chan usbthermal.RawFrame, error)
	// SubscribePictures feeds the encoded live view (video_usbthermal_live.go).
	SubscribePictures(ctx context.Context, stableID string) (<-chan usbthermal.Frame, error)
	LastError(stableID string) error
	Close()
}

func newUSBThermalDriver(ctx context.Context, logger *zap.Logger) usbThermalDriver {
	return usbthermal.NewManager(ctx, usbthermal.Options{
		Logger: logger,
		// A real /dev/videoN at a number in the band keeps it: the V4L2 node was
		// there first and resolveSource would otherwise route its id away.
		IDTaken: func(id uint32) bool {
			_, err := os.Lstat(fmt.Sprintf("/dev/video%d", id))
			return err == nil
		},
	})
}

// usbThermalDevices renders the attached USB thermal cameras for a listing.
func (s *VideoService) usbThermalDevices() []*agentpb.VideoDevice {
	if s.usbThermal == nil {
		return nil
	}
	listed := s.usbThermal.List()
	out := make([]*agentpb.VideoDevice, 0, len(listed))
	for _, l := range listed {
		out = append(out, &agentpb.VideoDevice{
			Id:        l.ID,
			Name:      l.Name,
			Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB,
			// The kernel driver the interfaces are bound to while the agent
			// streams them: usbfs, i.e. a userspace driver.
			Driver:   "usbfs",
			Model:    l.Model,
			StableId: l.StableID,
			Online:   true,
			// Declares the camera thermal and names the adapter its raw
			// frames' thermal_plane uses, as probeThermalCamera does for a
			// V4L2 thermal camera.
			Thermal: usbThermalProfile(l.Layout),
		})
	}
	return out
}

// usbThermalListing returns the attached camera with this stable id.
func (s *VideoService) usbThermalListing(stableID string) (usbthermal.Listing, bool) {
	if s.usbThermal == nil {
		return usbthermal.Listing{}, false
	}
	for _, l := range s.usbThermal.List() {
		if l.StableID == stableID {
			return l, true
		}
	}
	return usbthermal.Listing{}, false
}

// resolveUSBThermal maps a device id in usbthermal's band to the attached
// camera holding it. An id in the band that no such camera holds is not
// claimed, and resolves to /dev/videoN exactly as before.
func (s *VideoService) resolveUSBThermal(devID uint32) (videoSource, bool) {
	if s.usbThermal == nil || devID < usbthermal.IDBandStart || devID > usbthermal.IDBandEnd {
		return videoSource{}, false
	}
	for _, l := range s.usbThermal.List() {
		if l.ID == devID {
			return videoSource{kind: sourceUSBThermal, key: usbThermalHubKeyPrefix + l.StableID}, true
		}
	}
	return videoSource{}, false
}

// resolveUSBThermalStableID maps a stable id to the device id of the attached
// USB thermal camera that reports it. Exact match, like the V4L2 resolution.
func (s *VideoService) resolveUSBThermalStableID(stableID string) (uint32, bool) {
	if l, ok := s.usbThermalListing(strings.TrimSpace(stableID)); ok {
		return l.ID, true
	}
	return 0, false
}

// usbThermalStableIDs lists the attached cameras' stable ids, for the error
// that tells a caller which names exist.
func (s *VideoService) usbThermalStableIDs() []string {
	if s.usbThermal == nil {
		return nil
	}
	var out []string
	for _, l := range s.usbThermal.List() {
		out = append(out, l.StableID)
	}
	return out
}

// controlCameraPath is localCameraPath for the camera-control RPCs, refusing
// a USB thermal camera: it has no V4L2 controls, and its id would otherwise be
// read as a /dev/videoN that does not exist (or, worse, is another camera).
func (s *VideoService) controlCameraPath(devID uint32) (string, error) {
	if _, ok := s.resolveUSBThermal(devID); ok {
		return "", status.Errorf(codes.InvalidArgument,
			"camera %d is a USB thermal camera driven from userspace; it has no V4L2 controls", devID)
	}
	return localCameraPath(devID)
}

// usbThermalRawFormat is how the raw tap describes a camera's frames: the
// stacked layout of the original TC001's 256x384 UVC mode, picture rows on
// top and 1/64 K temperature rows below, so a consumer that decodes one decodes
// the other.
func usbThermalRawFormat(l usbthermal.Layout) *agentpb.RawFormat {
	return &agentpb.RawFormat{
		Width:        uint32(l.Width),
		Height:       uint32(2 * l.Height),
		Fourcc:       usbthermal.RawFourCC,
		BytesPerLine: uint32(l.RowBytes()),
	}
}

// usbThermalProfile is the thermal capability of a USB thermal camera, or
// nil for a layout no thermal adapter describes. The TC001 Max is the only
// model usbthermal drives today; a model added there needs its own adapter in
// data/thermal before it is declared here.
func usbThermalProfile(l usbthermal.Layout) *agentpb.ThermalCamera {
	format := usbThermalRawFormat(l)
	if _, err := thermal.Plane(thermal.TOPDONTC001Max, format); err != nil {
		return nil
	}
	return &agentpb.ThermalCamera{Adapter: thermal.TOPDONTC001Max, CaptureWidth: format.GetWidth(), CaptureHeight: format.GetHeight()}
}

// streamUSBThermal serves one StreamVideo call for a USB thermal camera.
func (s *VideoService) streamUSBThermal(stream grpc.ServerStreamingServer[agentpb.VideoFrame], src videoSource, req *agentpb.StreamVideoRequest) error {
	stableID := strings.TrimPrefix(src.key, usbThermalHubKeyPrefix)
	l, ok := s.usbThermalListing(stableID)
	if !ok {
		return status.Errorf(codes.NotFound, "camera %s is no longer connected", stableID)
	}
	if req.GetCodec() != agentpb.VideoCodec_VIDEO_CODEC_RAW {
		// An encoded request is the live view's (StreamVideo routes it there
		// first); reaching here means the camera could not be registered for
		// it, and the agent log says why.
		return status.Errorf(codes.FailedPrecondition,
			"%s: its encoded live view is not available (see the agent log); request raw frames "+
				"(VIDEO_CODEC_RAW, e.g. `wendy device camera view --id %d --raw --stdout`)", l.Name, l.ID)
	}
	format := usbThermalRawFormat(l.Layout)
	if w, h := req.GetWidth(), req.GetHeight(); (w != 0 || h != 0) && (w != format.GetWidth() || h != format.GetHeight()) {
		return status.Errorf(codes.InvalidArgument, "%s delivers %dx%d raw frames only; ask for that size or 0x0",
			l.Name, format.GetWidth(), format.GetHeight())
	}
	if req.GetFramerate() != 0 {
		return status.Errorf(codes.InvalidArgument, "%s runs at its own fixed frame rate; ask for framerate 0", l.Name)
	}
	// One hub per camera, joined by every raw subscriber whatever it asked:
	// the camera has exactly one mode.
	shared := &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW}
	h, id, ch, err := s.getOrCreateHub(stream.Context(), src.key, shared, hubHolderStreamClient)
	if err != nil {
		return err
	}
	defer h.unsubscribe(id)
	// Adaptive pacing is per subscriber, as for a V4L2 thermal camera.
	return s.pumpFrames(stream, h, id, ch, req.GetAdaptiveFramerate())
}

// runUSBThermalProducer feeds a hub from a USB thermal camera until the hub's
// context ends. Its subscription is what keeps the camera streaming: when the
// hub loses its last subscriber the context ends, the subscription with it,
// and the driver puts the camera back to idle.
func (s *VideoService) runUSBThermalProducer(ctx context.Context, h *deviceHub, stableID string) error {
	l, ok := s.usbThermalListing(stableID)
	if !ok {
		h.rawNotOffered(fixedReason("the camera is no longer connected"))
		return status.Errorf(codes.Unavailable, "camera %s is no longer connected", stableID)
	}
	frames, err := s.usbThermal.SubscribeRaw(ctx, stableID)
	if err != nil {
		h.rawNotOffered(fixedReason(err.Error()))
		return status.Errorf(codes.Unavailable, "%s: %v", l.Name, err)
	}
	// Every frame carries the thermal_plane that says where the temperatures
	// are and how to convert them (data/thermal), as publishRaw adds for a
	// V4L2 thermal camera.
	h.thermalProfile = usbThermalProfile(l.Layout)
	format := describeThermalCapture(h.thermalProfile, usbThermalRawFormat(l.Layout))
	h.rawOffered(format)
	s.logger.Info("streaming USB thermal camera", zap.String("camera", stableID),
		zap.Uint32("id", l.ID), zap.String("raw_format", describeRawFormat(format)))

	firstFrame, stall := usbThermalFirstFrameTimeout, usbThermalStallTimeout
	wait, waitFor := time.NewTimer(firstFrame), firstFrame
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wait.C:
			why := "no error recorded"
			if err := s.usbThermal.LastError(stableID); err != nil {
				why = err.Error()
			}
			return status.Errorf(codes.Unavailable, "%s delivered no thermal frame for %s: %s", l.Name, waitFor, why)
		case f, ok := <-frames:
			if !ok {
				return nil // the subscription ended with ctx
			}
			stamp := realtimeFrameTimestamp(f.Timestamp)
			if !h.produce(&videoFrame{
				data: f.Data, tsNs: stamp.wallNs, codec: agentpb.VideoCodec_VIDEO_CODEC_RAW,
				nativeNs: stamp.nativeNs, nativeClock: stamp.nativeClock, rawFmt: format,
			}) {
				return nil // no subscribers left
			}
			wait.Reset(stall)
			waitFor = stall
		}
	}
}
