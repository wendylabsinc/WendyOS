package rosmsg

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// TypeImage is the DDS type name a sensor_msgs/msg/Image writer advertises over SEDP.
const TypeImage = "sensor_msgs::msg::dds_::Image_"

// ImageHeader is the descriptive part of sensor_msgs/msg/Image: what the frame actually
// is, without the pixels. Everything inspection needs sits before the data array, so a
// frame is read without copying a megabyte of it.
type ImageHeader struct {
	FrameID  string
	Height   uint32
	Width    uint32
	Encoding string
	Step     uint32
	// StampSec and StampNanosec are the capture time the publisher stamped, which is
	// what frame age is measured against.
	StampSec     int32
	StampNanosec uint32
}

// Resolution renders the frame's real resolution, which is the measured counterpart to
// whatever a CameraInfo says its intrinsics were calibrated at. The two differing is the
// case that made a 640x480 calibration get quoted against an 848x480 stream.
func (h ImageHeader) Resolution() string {
	return fmt.Sprintf("%dx%d", h.Width, h.Height)
}

// DecodeImageHeader decodes a sensor_msgs/msg/Image up to the encoding and step, leaving
// the pixel data unread.
func DecodeImageHeader(payload []byte) (*ImageHeader, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, err
	}

	var header ImageHeader
	if header.StampSec, err = d.Int32(); err != nil {
		return nil, fmt.Errorf("header.stamp.sec: %w", err)
	}
	if header.StampNanosec, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("header.stamp.nanosec: %w", err)
	}
	if header.FrameID, err = d.String(); err != nil {
		return nil, fmt.Errorf("header.frame_id: %w", err)
	}
	if header.Height, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("height: %w", err)
	}
	if header.Width, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("width: %w", err)
	}
	if header.Encoding, err = d.String(); err != nil {
		return nil, fmt.Errorf("encoding: %w", err)
	}
	if _, err := d.Uint8(); err != nil {
		return nil, fmt.Errorf("is_bigendian: %w", err)
	}
	if header.Step, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("step: %w", err)
	}
	if header.Width == 0 || header.Height == 0 {
		return nil, fmt.Errorf("rosmsg: image on %q reports no resolution", header.FrameID)
	}
	return &header, nil
}
