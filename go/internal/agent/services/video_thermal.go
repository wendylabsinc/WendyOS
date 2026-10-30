package services

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

// thermalUSBIdentity walks the resolved video interface's sysfs parents to the
// USB device. Generic UVC display names are not enough to identify a TC001.
var thermalUSBIdentity = func(path string) (vendor, product string) {
	dir, err := filepath.EvalSymlinks(filepath.Join("/sys/class/video4linux", filepath.Base(path), "device"))
	if err != nil {
		return "", ""
	}
	for ; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		v, ve := os.ReadFile(filepath.Join(dir, "idVendor"))
		p, pe := os.ReadFile(filepath.Join(dir, "idProduct"))
		if ve == nil && pe == nil {
			return strings.TrimSpace(string(v)), strings.TrimSpace(string(p))
		}
	}
	return "", ""
}

// Probe both identity and exact capture modes. USB bridge IDs can be shared
// across products, so an ID alone must not enable a measurement decoder.
func probeThermalCamera(path, name string) *pb.ThermalCamera {
	adapter := thermal.Identify(name, nil)
	vendor, product := thermalUSBIdentity(path)
	if vendor == "2bdf" && product == "0102" && deviceSupportsRawSize(path, v4l2PixFmtYUYV, 256, 392) {
		return &pb.ThermalCamera{Adapter: thermal.TOPDONTC001HIK, CaptureWidth: 256, CaptureHeight: 392}
	}
	if (adapter == thermal.TOPDONTC001 || vendor == "0bda" && product == "5830") && deviceSupportsRawSize(path, v4l2PixFmtYUYV, 256, 384) {
		return &pb.ThermalCamera{Adapter: thermal.TOPDONTC001, CaptureWidth: 256, CaptureHeight: 384}
	}
	if adapter == thermal.PureThermal {
		for _, size := range [][2]uint32{{160, 120}, {80, 60}} {
			if deviceSupportsRawSize(path, v4l2PixFmtY16, size[0], size[1]) {
				return &pb.ThermalCamera{Adapter: adapter, CaptureWidth: size[0], CaptureHeight: size[1]}
			}
		}
	}
	return nil
}

var thermalCaptureProfile = func(path string) *pb.ThermalCamera {
	name, _ := os.ReadFile(filepath.Join("/sys/class/video4linux", filepath.Base(path), "name"))
	return probeThermalCamera(path, strings.TrimSpace(string(name)))
}

func thermalPreviewCrop(profile *pb.ThermalCamera, width, height uint32) (top, bottom uint32, ok bool) {
	if width != 256 {
		return 0, 0, false
	}
	switch profile.GetAdapter() {
	case thermal.TOPDONTC001:
		if height == 384 {
			return 0, 192, true
		}
	case thermal.TOPDONTC001HIK:
		if height == 392 {
			return 196, 4, true
		}
	}
	return 0, 0, false
}

func describeThermalCapture(profile *pb.ThermalCamera, layout *pb.RawFormat) *pb.RawFormat {
	if profile == nil || layout == nil {
		return layout
	}
	plane, err := thermal.Plane(profile.Adapter, layout)
	if err != nil {
		return layout
	}
	out := proto.Clone(layout).(*pb.RawFormat)
	out.ThermalPlane = plane
	return out
}

// The Data decoder measures the original sensor bytes, before the viewer's
// palette or H.264 encoding. No temperature calibration is inferred here.
func rawFrameExtrema(frame *videoFrame) *pb.RawFrameExtrema {
	if frame == nil || frame.codec != pb.VideoCodec_VIDEO_CODEC_RAW || frame.rawFmt == nil {
		return nil
	}
	layout := frame.rawFmt
	if layout.GetThermalPlane() == nil {
		return nil
	}
	f, err := thermal.Decode(frame.data, layout)
	if err != nil {
		return nil
	}
	return &pb.RawFrameExtrema{ColdX: f.Coldest.X, ColdY: f.Coldest.Y, ColdRaw: uint32(f.Coldest.Raw), HotX: f.Hottest.X, HotY: f.Hottest.Y, HotRaw: uint32(f.Hottest.Raw)}
}
