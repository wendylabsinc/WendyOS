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
	if layout.GetThermalPlane().GetAdapter() != TOPDONTC001HIK ||
		layout.GetFourcc() != "YUYV" || layout.GetWidth() != 256 || layout.GetHeight() != 392 ||
		layout.GetBytesPerLine() != 512 || len(data) != 200704 ||
		binary.LittleEndian.Uint32(data[193*512:]) != 0xaabbccdd {
		return nil
	}
	var words [10]uint16
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(data[388*512+i*2:])
	}
	if words[0] != 0xffff || words[1] != 0xeeee || words[2] == 0 || words[5] == 0 ||
		words[2] == 65535 || words[5] == 65535 || words[2] < words[5] ||
		words[3] > 1000 || words[4] > 1000 || words[6] > 1000 || words[7] > 1000 {
		return nil
	}
	point := func(code, x, y uint16) Point {
		return Point{X: (uint32(x)*255 + 500) / 1000, Y: (uint32(y)*191 + 500) / 1000, Raw: code}
	}
	return &TemperatureSummary{Coldest: point(words[5], words[6], words[7]), Hottest: point(words[2], words[3], words[4]),
		ColdCelsius: float64(words[5])/64 - 50, HotCelsius: float64(words[2])/64 - 50, Identity: words}
}
