// Package thermal decodes sensor measurements without interpreting display
// brightness as temperature. It is shared by Wendy Data and native clients.
package thermal

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"strings"
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
	if stride < width*2 || stride > 16384 || uint64(stride)*uint64(height) != uint64(len(payload)) {
		return nil, fmt.Errorf("invalid thermal frame stride or payload length")
	}
	f := &Frame{Width: width, Height: height, Pixels: make([]uint16, int(width*height))}
	found := false
	for y := uint32(0); y < height; y++ {
		for x := uint32(0); x < width; x++ {
			v := binary.LittleEndian.Uint16(payload[int(y*stride+x*2):])
			f.Pixels[y*width+x] = v
			if v == 0 || v == 65535 {
				continue
			}
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
		if v != 0 && v != 65535 {
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
