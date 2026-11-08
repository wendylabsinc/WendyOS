// Package thermal decodes sensor measurements without interpreting display
// brightness as temperature. It is shared by Wendy Data and native clients.
package thermal

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Point uses zero-based sensor coordinates, before any display scaling.
type Point struct {
	X, Y uint32
	Raw  uint16
}

type Frame struct {
	Width, Height    uint32
	Coldest, Hottest Point
	Pixels           []uint16
	Valid            []bool
	Temperature      *pb.ThermalLinearTemperature
}

// IsPureThermal identifies the camera family, not radiometric calibration.
// Y16 can contain either radiometric TLinear values or uncalibrated counts.
func IsPureThermal(name string, formats []string) bool {
	name = strings.ToLower(name)
	if !strings.Contains(name, "purethermal") && !strings.Contains(name, "pure thermal") {
		return false
	}
	if len(formats) == 0 {
		return true
	} // Older agents did not report formats.
	for _, f := range formats {
		if f == "Y16 " {
			return true
		}
	}
	return false
}

// DecodePureThermal reads little-endian Y16, honoring stride. Only picture-only
// Lepton modes are accepted; telemetry rows must never participate in extrema.
// Zero and 0xffff are treated as invalid samples. Ties use row-major order.
func DecodePureThermal(payload []byte, fourcc string, width, height, stride uint32) (*Frame, error) {
	if fourcc != "Y16 " {
		return nil, fmt.Errorf("thermal measurements require Y16; received %q", fourcc)
	}
	if !((width == 160 && height == 120) || (width == 80 && height == 60)) {
		return nil, fmt.Errorf("unsupported PureThermal picture size %dx%d; use 160x120 or 80x60 without telemetry rows", width, height)
	}
	return Decode(payload, &pb.RawFormat{Fourcc: fourcc, Width: width, Height: height, BytesPerLine: stride,
		ThermalPlane: &pb.ThermalPlane{Version: 1, Adapter: PureThermal, Width: width, Height: height, RowStrideBytes: stride, InvalidValues: []uint32{0, 65535}}})
}

// Decode consumes a described measurement plane, independently of camera brand.
// Capture bytes remain untouched; returned samples use sensor coordinates.
func Decode(payload []byte, layout *pb.RawFormat) (*Frame, error) {
	p := layout.GetThermalPlane()
	if p == nil || p.GetVersion() != 1 {
		return nil, fmt.Errorf("missing or unsupported thermal measurement-plane version")
	}
	w, h, stride, offset := uint64(p.Width), uint64(p.Height), uint64(p.RowStrideBytes), uint64(p.OffsetBytes)
	if w == 0 || h == 0 || w > 4096 || h > 4096 || w*h > 4_000_000 || stride < w*2 || stride > 65536 ||
		uint64(layout.GetBytesPerLine())*uint64(layout.GetHeight()) != uint64(len(payload)) ||
		offset+(h-1)*stride+w*2 > uint64(len(payload)) || len(p.InvalidValues) > 256 {
		return nil, fmt.Errorf("invalid thermal plane geometry, stride or payload length")
	}
	if t := p.Temperature; t != nil {
		if t.KelvinPerCount <= 0 || math.IsNaN(t.KelvinPerCount) || math.IsInf(t.KelvinPerCount, 0) ||
			math.IsNaN(t.KelvinOffset) || math.IsInf(t.KelvinOffset, 0) || t.Provenance == "" ||
			math.IsInf(65535*t.KelvinPerCount+t.KelvinOffset, 0) {
			return nil, fmt.Errorf("invalid thermal temperature conversion")
		}
	}
	invalid := make(map[uint16]bool, len(p.InvalidValues))
	for _, v := range p.InvalidValues {
		if v > 65535 {
			return nil, fmt.Errorf("invalid thermal sentinel %d", v)
		}
		invalid[uint16(v)] = true
	}
	f := &Frame{Width: p.Width, Height: p.Height, Pixels: make([]uint16, int(w*h)), Valid: make([]bool, int(w*h)), Temperature: p.Temperature}
	found := false
	for y := uint32(0); y < p.Height; y++ {
		for x := uint32(0); x < p.Width; x++ {
			v := binary.LittleEndian.Uint16(payload[int(offset+uint64(y)*stride+uint64(x)*2):])
			f.Pixels[y*p.Width+x] = v
			if invalid[v] {
				continue
			}
			f.Valid[y*p.Width+x] = true
			p := Point{X: x, Y: y, Raw: v}
			if !found || v < f.Coldest.Raw {
				f.Coldest = p
			}
			if !found || v > f.Hottest.Raw {
				f.Hottest = p
			}
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("thermal frame contains no valid sensor samples")
	}
	return f, nil
}

// Image maps the current measured range to a fixed black-purple-orange-yellow
// palette. Invalid pixels are gray. The palette never affects measurements.
func (f *Frame) Image() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, int(f.Width), int(f.Height)))
	stops := []color.RGBA{{0, 0, 0, 255}, {71, 16, 112, 255}, {187, 55, 84, 255}, {249, 142, 9, 255}, {252, 255, 164, 255}}
	span := float64(f.Hottest.Raw - f.Coldest.Raw)
	for i, v := range f.Pixels {
		c := color.RGBA{80, 80, 80, 255}
		if f.Valid[i] {
			t := 0.5
			if span > 0 {
				t = float64(v-f.Coldest.Raw) / span
			}
			pos := t * float64(len(stops)-1)
			lo := min(int(pos), len(stops)-2)
			a, b := stops[lo], stops[lo+1]
			u := pos - float64(lo)
			mix := func(x, y uint8) uint8 { return uint8(float64(x)*(1-u) + float64(y)*u) }
			c = color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 255}
		}
		copy(img.Pix[i*4:i*4+4], []byte{c.R, c.G, c.B, c.A})
	}
	return img
}
