package thermal

import (
	"encoding/binary"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TemperatureSummary is a camera-reported measurement, separate from the
// sensor plane. HIK repeats it across video frames at roughly 2.5 Hz.
type TemperatureSummary struct {
	Coldest, Hottest        Point
	ColdCelsius, HotCelsius float64
	Identity                [10]uint16
}

// DecodeTemperatureSummary recognizes the HIK default-mode trailer. Its
// temperature codes use 1/64 degree Celsius with a -50 degree offset. This
// conversion MUST NOT be applied to the sensor pixels.
// Protocol evidence: github.com/Krastanov/ht-203u-linux/THERMOMETRY.md.
func DecodeTemperatureSummary(data []byte, layout *pb.RawFormat) *TemperatureSummary {
	if layout.GetThermalPlane().GetAdapter() != TOPDONTC001HIK || layout.GetFourcc() != "YUYV" {
		return nil
	}
	w, h := layout.GetWidth(), layout.GetHeight()
	if !((w == 256 && h == 392) || (w == 96 && h == 200)) {
		return nil
	}
	stride, sensorHeight := w*2, (h-8)/2
	if layout.GetBytesPerLine() != stride || len(data) != int(stride*h) {
		return nil
	}
	header := (sensorHeight + 1) * stride
	if binary.LittleEndian.Uint32(data[header:]) != 0xaabbccdd {
		return nil
	}
	// Verified on 2bdf:0102 / firmware 0409: the 96x96 variant carries
	// the same summary codes and normalized coordinates at row 196.
	// Successive summaries match the live sensor extrema; the initial
	// summary may be cached. Consumers must retain the freshness gate.
	if w == 96 && (binary.LittleEndian.Uint32(data[header+8:]) != w ||
		binary.LittleEndian.Uint32(data[header+12:]) != sensorHeight) {
		return nil
	}
	trailer := (h - 4) * stride
	var words [10]uint16
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(data[int(trailer)+i*2:])
	}
	if words[0] != 0xffff || words[1] != 0xeeee || words[2] == 0 || words[5] == 0 ||
		words[2] == 65535 || words[5] == 65535 || words[2] < words[5] ||
		words[3] > 1000 || words[4] > 1000 || words[6] > 1000 || words[7] > 1000 {
		return nil
	}
	point := func(code, x, y uint16) Point {
		return Point{X: (uint32(x)*(w-1) + 500) / 1000, Y: (uint32(y)*(sensorHeight-1) + 500) / 1000, Raw: code}
	}
	return &TemperatureSummary{Coldest: point(words[5], words[6], words[7]), Hottest: point(words[2], words[3], words[4]),
		ColdCelsius: float64(words[5])/64 - 50, HotCelsius: float64(words[2])/64 - 50, Identity: words}
}
