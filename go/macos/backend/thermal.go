package backend

import (
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

type ThermalFrame struct {
	Width, Height    uint32
	Coldest, Hottest thermal.Point
	TimestampNS      uint64
	RGBA             []byte
	Temperature      *pb.ThermalLinearTemperature
	Summary          *thermal.TemperatureSummary
	SummaryUpdatedNS uint64
	// Only legacy PureThermal frames allow the operator's TLinear override.
	AllowScaleOverride bool
}

func decodeThermalFrame(v *pb.VideoFrame, profile *pb.ThermalCamera) (*ThermalFrame, error) {
	if v.GetCodec() != pb.VideoCodec_VIDEO_CODEC_RAW || v.GetRawFormat() == nil {
		return nil, fmt.Errorf("this camera did not provide raw thermal measurements")
	}
	layout := v.GetRawFormat()
	if layout.GetThermalPlane() == nil {
		adapter := profile.GetAdapter()
		plane, err := thermal.Plane(adapter, layout)
		if err != nil {
			return nil, err
		}
		layout = proto.Clone(layout).(*pb.RawFormat)
		layout.ThermalPlane = plane
	}
	f, err := thermal.Decode(v.GetData(), layout)
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
	return &ThermalFrame{Width: f.Width, Height: f.Height, Coldest: f.Coldest, Hottest: f.Hottest, TimestampNS: v.GetTimestampNs(), RGBA: f.Image().Pix,
		Summary: thermal.DecodeTemperatureSummary(v.GetData(), layout), Temperature: f.Temperature, AllowScaleOverride: layout.GetThermalPlane().GetAdapter() == thermal.PureThermal && f.Temperature == nil}, nil
}

// Repeated metadata does not refresh the measurement age. Suppress the first
// summary until it changes because startup can return a cached reading.
type thermalSummaryTracker struct {
	previous [10]uint16
	seen     bool
	updated  time.Time
}

func (t *thermalSummaryTracker) apply(f *ThermalFrame, now time.Time) {
	if f.Summary == nil {
		return
	}
	if !t.seen {
		t.previous, t.seen = f.Summary.Identity, true
	} else if t.previous != f.Summary.Identity {
		t.previous, t.updated = f.Summary.Identity, now
	}
	if t.updated.IsZero() || now.Sub(t.updated) >= 3*time.Second {
		f.Summary = nil
		return
	}
	f.SummaryUpdatedNS = uint64(t.updated.UnixNano())
}
