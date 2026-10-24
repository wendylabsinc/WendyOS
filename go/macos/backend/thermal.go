package backend

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type ThermalFrame struct {
	Width, Height    uint32
	Coldest, Hottest thermal.Point
	TimestampNS      uint64
	RGBA             []byte
}

func decodeThermalFrame(v *pb.VideoFrame) (*ThermalFrame, error) {
	if v.GetCodec() != pb.VideoCodec_VIDEO_CODEC_RAW || v.GetRawFormat() == nil {
		return nil, fmt.Errorf("this camera did not provide raw thermal measurements; update the device agent to a version with Y16 support")
	}
	layout := v.GetRawFormat()
	f, err := thermal.DecodePureThermal(v.GetData(), layout.GetFourcc(), layout.GetWidth(), layout.GetHeight(), layout.GetBytesPerLine())
	if err != nil {
		return nil, err
	}
	// Older agents omit the summary. Both paths use the same Data decoder.
	if measured := v.GetRawExtrema(); measured != nil {
		if measured.GetColdX() != f.Coldest.X || measured.GetColdY() != f.Coldest.Y || measured.GetColdRaw() != uint32(f.Coldest.Raw) ||
			measured.GetHotX() != f.Hottest.X || measured.GetHotY() != f.Hottest.Y || measured.GetHotRaw() != uint32(f.Hottest.Raw) {
			return nil, fmt.Errorf("device thermal measurements do not match the raw frame")
		}
		f.Coldest = thermal.Point{X: measured.GetColdX(), Y: measured.GetColdY(), Raw: uint16(measured.GetColdRaw())}
		f.Hottest = thermal.Point{X: measured.GetHotX(), Y: measured.GetHotY(), Raw: uint16(measured.GetHotRaw())}
	}
	return &ThermalFrame{Width: f.Width, Height: f.Height, Coldest: f.Coldest, Hottest: f.Hottest, TimestampNS: v.GetTimestampNs(), RGBA: f.Image().Pix}, nil
}
