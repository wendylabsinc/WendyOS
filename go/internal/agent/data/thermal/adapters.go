package thermal

import (
	"fmt"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	PureThermal    = "purethermal-y16-v1"
	TOPDONTC001    = "topdon-tc001-v1"
	TOPDONTC001HIK = "topdon-tc001-hik-v1"
)

// Identify is a legacy name-based fallback. New agents also probe USB identity
// and supported modes. A generic UVC name or a FourCC alone proves nothing.
func Identify(name string, formats []string) string {
	if IsPureThermal(name, formats) {
		return PureThermal
	}
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, word := range words {
		if word == "tc001" && (len(formats) == 0 || containsFormat(formats, "YUYV")) {
			return TOPDONTC001
		}
	}
	return ""
}

func containsFormat(formats []string, want string) bool {
	for _, f := range formats {
		if f == want {
			return true
		}
	}
	return false
}

// CameraProfile prefers explicit agent capabilities. Unknown future adapters
// still work when the agent describes a supported thermal-plane version.
func CameraProfile(camera *pb.VideoDevice) *pb.ThermalCamera {
	if p := camera.GetThermal(); p != nil {
		return p
	}
	adapter := Identify(camera.GetName(), camera.GetPixelFormats())
	if adapter == "" {
		return nil
	}
	p := &pb.ThermalCamera{Adapter: adapter}
	if adapter == TOPDONTC001 {
		p.CaptureWidth, p.CaptureHeight = 256, 384
	}
	return p
}

// Plane describes only verified layouts. Never choose a plane by searching for
// plausible temperatures or by treating an entire YUYV image as uint16 values.
func Plane(adapter string, layout *pb.RawFormat) (*pb.ThermalPlane, error) {
	w, h, stride := layout.GetWidth(), layout.GetHeight(), layout.GetBytesPerLine()
	p := &pb.ThermalPlane{Version: 1, Adapter: adapter, Width: w, Height: h, RowStrideBytes: stride, InvalidValues: []uint32{0, 65535}}
	switch adapter {
	case PureThermal:
		if layout.GetFourcc() == "Y16 " && ((w == 80 && h == 60) || (w == 160 && h == 120)) {
			return p, nil
		}
	case TOPDONTC001:
		// TC001's original UVC mode has 192 YUYV preview rows followed by
		// 192 rows of little-endian uint16 temperatures in 1/64 kelvin.
		// Evidence: leswright1977/PyThermalCamera, src/tc001v4.2.py.
		if layout.GetFourcc() == "YUYV" && w == 256 && h == 384 && stride <= 65536 {
			p.Height, p.OffsetBytes = 192, 192*stride
			p.Temperature = &pb.ThermalLinearTemperature{KelvinPerCount: 1.0 / 64,
				Provenance: "TC001 UVC 256x384 temperature plane, 1/64 K"}
			return p, nil
		}
	case TOPDONTC001HIK:
		// HIK-based TC001 hardware uses sensor counts, not the older TC001's
		// temperature codes. Four metadata rows follow the 256x192 plane.
		// No temperature scale is established for this default capture mode.
		if layout.GetFourcc() == "YUYV" && w == 256 && h == 392 {
			p.Height = 192
			return p, nil
		}
	}
	return nil, fmt.Errorf("unsupported %s thermal layout %s %dx%d; no temperature conversion is available for this capture mode", adapter, layout.GetFourcc(), w, h)
}
