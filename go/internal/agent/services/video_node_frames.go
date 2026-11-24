package services

import (
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// nodeFrames is what a capture of a camera that names no parameters has for
// camera nodes: the camera's own frames, whole, and pictures. A camera type
// that captures differently changes what this reports, and no node code.
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
	// softwareEncoder names the software element this device makes H.264 with
	// from the camera's own frames; empty for a camera whose own frames need
	// no encoder and for an encoder not known to be software.
	softwareEncoder string
}

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

// framesForNodes asks the capture sources what a camera has for nodes. It
// captures nothing.
func (s *VideoService) framesForNodes(dev *agentpb.VideoDevice) nodeFrames {
	if refused := s.notCapturedForNodes(dev); refused != "" {
		return nodeFrames{refused: refused}
	}
	gstPath, available, err := s.gstElements()
	if err != nil {
		return nodeFrames{refused: "GStreamer is not installed on this device"}
	}
	var frames nodeFrames
	frames.own, frames.ownDeclined, frames.native = s.ownFramesFor(dev.GetPath(), &agentpb.StreamVideoRequest{})
	switch {
	case !frames.native:
		frames.pictures, frames.pictureRate, frames.picturesWhyNot = tappedPictures(dev.GetPath(), gstPath, available)
	case frames.ownDeclined == "":
		// The layer that decodes the pictures has chosen the encoder too. A
		// camera with no pictures has no layer to ask.
		if layer, whyNot := s.pictureLayerFor(nil, dev.GetPath(), frames.own); layer != nil {
			frames.pictures, frames.softwareEncoder = layer.pictures, layer.softwareEncoder()
		} else {
			frames.picturesWhyNot, frames.softwareEncoder = whyNot, softwareEncoderOfH264From(frames.own.codec, available)
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
