package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The Data decoder measures the original sensor bytes, before the viewer's
// palette or H.264 encoding. No temperature calibration is inferred here.
func rawFrameExtrema(frame *videoFrame) *pb.RawFrameExtrema {
	if frame == nil || frame.codec != pb.VideoCodec_VIDEO_CODEC_RAW || frame.rawFmt == nil {
		return nil
	}
	layout := frame.rawFmt
	f, err := thermal.DecodePureThermal(frame.data, layout.GetFourcc(), layout.GetWidth(), layout.GetHeight(), layout.GetBytesPerLine())
	if err != nil {
		return nil
	}
	return &pb.RawFrameExtrema{ColdX: f.Coldest.X, ColdY: f.Coldest.Y, ColdRaw: uint32(f.Coldest.Raw), HotX: f.Hottest.X, HotY: f.Hottest.Y, HotRaw: uint32(f.Hottest.Raw)}
}
