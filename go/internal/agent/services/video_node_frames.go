package services

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/camerashare"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// nodeFrames is what a capture of a camera has for camera nodes: the camera's
// own frames, whole, and pictures. A camera type that captures differently
// changes what this reports, and no node code.
type nodeFrames struct {
	// refused is why no capture of this camera feeds a node.
	refused string
	// own are the camera's own frames; ownDeclined is why none are handed on.
	// native says a native source captures the camera either way, so its
	// pictures are decoded from its own frames, not tapped from a pipeline.
	own         ownFrames
	ownDeclined string
	native      bool
	// pictures and pictureRate describe its pictures; picturesWhyNot is why
	// it has none.
	pictures       *agentpb.RawFormat
	pictureRate    uint32
	picturesWhyNot string
	// h264Encoder names the element this device makes H.264 with from the
	// camera's own frames; empty for a camera whose own frames need no encoder
	// and for a device that cannot encode them.
	h264Encoder string
}

func (f nodeFrames) softwareEncoder() string { return softwareEncoderAmong(f.h264Encoder) }

// isDepthOrInfraredNode is behind a var so tests can describe a RealSense node
// without sysfs.
var isDepthOrInfraredNode = isRealSenseDepthNode

// notCapturedForNodes is why the agent's capture of a camera has nothing for a
// node, or "": the captures that hand on whole frames or tap pictures are those
// of USB cameras that are not depth, infrared or thermal sensors.
func (s *VideoService) notCapturedForNodes(dev *agentpb.VideoDevice) string {
	switch dev.GetTransport() {
	case agentpb.VideoTransport_VIDEO_TRANSPORT_USB:
	case agentpb.VideoTransport_VIDEO_TRANSPORT_CSI:
		return "CSI cameras are not shared yet"
	default:
		return "only USB cameras are shared in this release"
	}
	if isDepthOrInfraredNode(dev.GetPath()) {
		return "depth and infrared nodes are not shared"
	}
	if dev.GetThermal() != nil {
		return "thermal cameras are not shared; an app reads their measurements at a mode it picks"
	}
	var formats []uint32
	if s.capturePixelFormats != nil {
		formats, _ = s.capturePixelFormats(dev.GetPath())
	}
	if len(formats) == 0 {
		return "the camera's formats could not be read"
	}
	return ""
}

// noSize asks a camera's capture for no size: the capture chooses one.
var noSize = appconfig.CameraSize{}

const ownFramesNotWholeAt = "the camera's own frames are not captured whole at %s: %s"

// ownFramesAt is what a capture asked for size hands on of the camera's own
// frames, or why it hands on none whole. Only a capture that does serves a
// size an entitlement states.
func (s *VideoService) ownFramesAt(path string, size appconfig.CameraSize) (own ownFrames, whyNot string) {
	own, declined, native := s.ownFramesFor(path, &agentpb.StreamVideoRequest{Width: size.Width, Height: size.Height})
	if !native || declined != "" {
		return ownFrames{}, fmt.Sprintf(ownFramesNotWholeAt, size, declined)
	}
	return own, ""
}

// ownFrameSizes lists the sizes the camera offers its own frames of codec in.
func ownFrameSizes(path string, codec agentpb.VideoCodec) [][2]uint32 {
	return enumerateRawFrameSizes(path, compressedNodeFormats[codec].v4l2)
}

// statableSizes lists the sizes an entitlement can state for the camera's
// node, smallest first, each with the rate its capture runs at there, or says
// why none can be stated.
func (s *VideoService) statableSizes(dev *agentpb.VideoDevice) (sizes []camerashare.SizeAtRate, whyNone string) {
	chosen, declined, native := s.ownFramesFor(dev.GetPath(), &agentpb.StreamVideoRequest{})
	if !native || declined != "" {
		return nil, fmt.Sprintf(ownFramesNotWhole, declined)
	}
	for _, listed := range ownFrameSizes(dev.GetPath(), chosen.codec) {
		own, whyNot := s.ownFramesAt(dev.GetPath(), appconfig.CameraSize{Width: listed[0], Height: listed[1]})
		if whyNot != "" {
			continue
		}
		size := camerashare.SizeAtRate{Width: listed[0], Height: listed[1], FPS: own.framerate}
		if size.FPS == 0 {
			size.FPS = defaultSharedFPS
		}
		if !slices.Contains(sizes, size) {
			sizes = append(sizes, size)
		}
	}
	if len(sizes) == 0 {
		return nil, "the camera lists no size its own frames are captured whole at"
	}
	slices.SortFunc(sizes, func(a, b camerashare.SizeAtRate) int {
		return cmp.Or(cmp.Compare(uint64(a.Width)*uint64(a.Height), uint64(b.Width)*uint64(b.Height)), cmp.Compare(a.Width, b.Width))
	})
	return sizes, ""
}

// framesForNodes asks the capture sources what a camera captured at size, or
// at the size its capture chooses for noSize, has for nodes. It captures
// nothing.
func (s *VideoService) framesForNodes(dev *agentpb.VideoDevice, size appconfig.CameraSize) nodeFrames {
	if refused := s.notCapturedForNodes(dev); refused != "" {
		return nodeFrames{refused: refused}
	}
	gstPath, available, err := s.gstElements()
	if err != nil {
		return nodeFrames{refused: "GStreamer is not installed on this device"}
	}
	var frames nodeFrames
	if size == noSize {
		frames.own, frames.ownDeclined, frames.native = s.ownFramesFor(dev.GetPath(), &agentpb.StreamVideoRequest{})
	} else {
		own, whyNot := s.ownFramesAt(dev.GetPath(), size)
		if whyNot == "" && !slices.Contains(ownFrameSizes(dev.GetPath(), own.codec), [2]uint32{size.Width, size.Height}) {
			whyNot = fmt.Sprintf("the camera lists no %s at %s", compressedNodeFormats[own.codec].name, size)
		}
		if whyNot != "" {
			return nodeFrames{refused: whyNot}
		}
		frames.own, frames.native = own, true
	}
	switch {
	case !frames.native:
		frames.pictures, frames.pictureRate, frames.picturesWhyNot = tappedPictures(dev.GetPath(), gstPath, available)
	case frames.ownDeclined == "":
		// The layer that decodes the pictures has chosen the encoder too. A
		// camera with no pictures has no layer to ask.
		if layer, whyNot := s.pictureLayerFor(nil, dev.GetPath(), frames.own); layer != nil {
			frames.pictures, frames.h264Encoder = layer.pictures, layer.h264Encoder()
		} else {
			frames.picturesWhyNot, frames.h264Encoder = whyNot, h264EncoderFrom(frames.own.codec, available)
		}
		frames.pictureRate = frames.own.framerate
	}
	return frames
}

// decodedPictures are the pictures the derived layer makes from a natively
// captured camera's frames, or why it makes none.
func (s *VideoService) decodedPictures(path string, own ownFrames) (pictures *agentpb.RawFormat, whyNot string) {
	layer, whyNot := s.pictureLayerFor(nil, path, own)
	if layer == nil {
		return nil, whyNot
	}
	return layer.pictures, ""
}

// tappedPictures are the pictures a pipeline capture of a camera tees to its
// tap, and the rate it captures at, or why it has none.
func tappedPictures(path, gstPath string, available map[string]bool) (pictures *agentpb.RawFormat, rate uint32, whyNot string) {
	plan, err := planGStreamerPipelineShaped(pipelineShape{encode: false}, gstPath, path,
		&agentpb.StreamVideoRequest{}, "", false, camera.TransportUSB, "", pipeWireSource{}, available)
	if err != nil && !errors.Is(err, errNoTapOnlyPipeline) {
		return nil, 0, "no shared mode: " + err.Error()
	}
	if plan.pixels == nil {
		return nil, 0, "no shared mode: " + plan.rawReason()
	}
	captureFormat := uint32(v4l2PixFmtMJPEG)
	if plan.raw != nil {
		for _, f := range rawPixelFormats {
			if f.fourcc == plan.raw.GetFourcc() {
				captureFormat = f.v4l2
			}
		}
	}
	return plan.pixels, highestFrameRateFor(path, captureFormat, plan.pixels.GetWidth(), plan.pixels.GetHeight()), ""
}

const (
	ownFramesNotWhole     = "the camera's own frames are not captured whole: %s"
	ownFramesNotDescribed = "the camera's own frames cannot be declared before they are captured: %s"
)

// nodeCodec is the codec of the frames a compressed node of format carries from
// this camera: its own, or the H.264 made from them.
func (f nodeFrames) nodeCodec(format string) agentpb.VideoCodec {
	if fixed, ok := shareFormatCodecs[format]; ok {
		return fixed
	}
	return f.own.codec
}

// whyNo is why the camera has no frames for a node of format, or "".
func (f nodeFrames) whyNo(format string) string {
	pictures := formatCarriesPictures(format)
	switch {
	case f.refused != "":
		return f.refused
	case f.native && f.ownDeclined != "":
		return fmt.Sprintf(ownFramesNotDescribed, f.ownDeclined)
	case pictures && f.pictures == nil:
		return f.picturesWhyNot
	case !pictures && f.ownDeclined != "":
		return fmt.Sprintf(ownFramesNotWhole, f.ownDeclined)
	}
	return ""
}
