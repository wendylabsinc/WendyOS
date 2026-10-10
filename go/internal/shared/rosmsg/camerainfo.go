// Package rosmsg decodes the ROS 2 messages robot inspection reads. Like the agent's
// rosbattery decoders it knows message layouts and nothing about transport, so every
// function here is a pure function of bytes and testable without a robot.
package rosmsg

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// TypeCameraInfo is the DDS type name a sensor_msgs/msg/CameraInfo writer advertises
// over SEDP.
const TypeCameraInfo = "sensor_msgs::msg::dds_::CameraInfo_"

// CameraInfo is the part of sensor_msgs/msg/CameraInfo that inspection uses: the frame
// the camera reports in, the resolution the intrinsics were calibrated at, and the
// focal lengths and principal point from the K matrix.
type CameraInfo struct {
	FrameID         string
	Height          uint32
	Width           uint32
	DistortionModel string
	// FX and FY are K[0] and K[4], the focal lengths in pixels. An uncalibrated
	// camera publishes zeros here, which is why FieldOfView refuses to guess.
	FX float64
	FY float64
	// CX and CY are K[2] and K[5], the principal point.
	CX float64
	CY float64
}

// DecodeCameraInfo decodes a sensor_msgs/msg/CameraInfo CDR payload. Fields past the K
// matrix are not read: rectification and projection matter to a consumer of images, not
// to an inventory of what the camera claims about itself.
func DecodeCameraInfo(payload []byte) (*CameraInfo, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, err
	}

	// std_msgs/Header: stamp {sec, nanosec} then frame_id.
	if _, err := d.Int32(); err != nil {
		return nil, fmt.Errorf("header.stamp.sec: %w", err)
	}
	if _, err := d.Uint32(); err != nil {
		return nil, fmt.Errorf("header.stamp.nanosec: %w", err)
	}
	frameID, err := d.String()
	if err != nil {
		return nil, fmt.Errorf("header.frame_id: %w", err)
	}

	info := CameraInfo{FrameID: frameID}
	if info.Height, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("height: %w", err)
	}
	if info.Width, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("width: %w", err)
	}
	if info.DistortionModel, err = d.String(); err != nil {
		return nil, fmt.Errorf("distortion_model: %w", err)
	}
	// d is a sequence whose length depends on the distortion model.
	if err := d.SkipFloat64Seq(); err != nil {
		return nil, fmt.Errorf("d: %w", err)
	}

	// k is a fixed nine-element array, so it carries no length prefix.
	var k [9]float64
	for i := range k {
		if k[i], err = d.Float64(); err != nil {
			return nil, fmt.Errorf("k[%d]: %w", i, err)
		}
	}
	info.FX, info.CX, info.FY, info.CY = k[0], k[2], k[4], k[5]
	return &info, nil
}

// FieldOfView returns the horizontal and vertical field of view in degrees, derived
// from the focal lengths and the calibrated resolution.
//
// It returns an error rather than a number when the camera is uncalibrated. A
// RealSense publishes an all-zero K until it has been calibrated, and 2·atan(w/0) is
// 180 degrees — a plausible-looking figure with nothing behind it. Refusing here is
// what keeps that out of a report as a measured value.
func (c CameraInfo) FieldOfView() (horizontal, vertical float64, err error) {
	if c.FX <= 0 || c.FY <= 0 {
		return 0, 0, fmt.Errorf("rosmsg: camera %q publishes no intrinsics (fx=%g fy=%g); it is uncalibrated", c.FrameID, c.FX, c.FY)
	}
	if c.Width == 0 || c.Height == 0 {
		return 0, 0, fmt.Errorf("rosmsg: camera %q reports no resolution", c.FrameID)
	}
	horizontal = degrees(2 * math.Atan(float64(c.Width)/(2*c.FX)))
	vertical = degrees(2 * math.Atan(float64(c.Height)/(2*c.FY)))
	return horizontal, vertical, nil
}

// Resolution renders the resolution the intrinsics belong to. Any field of view derived
// from them is only true at this resolution, so it travels with the value as a
// condition rather than being dropped.
func (c CameraInfo) Resolution() string {
	return fmt.Sprintf("%dx%d", c.Width, c.Height)
}

func degrees(radians float64) float64 { return radians * 180 / math.Pi }
