// Package ros2sensor publishes the generic sensor channels of paired
// SensorLink sources as ROS 2 topics inside every ROS 2 app on the device.
//
// Cameras and microphones are delivered to apps as devices, behind the camera
// and audio entitlements. A generic sensor channel (a temperature, an IMU) has
// no device to appear as, so it is published instead: one topic per channel,
// /sensorlink/<source>/<channel>.
package ros2sensor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// schemaTemperature is the one sensor schema with a standard ROS 2 message:
// one little-endian float32 per sample, in degrees Celsius.
const schemaTemperature = "wendy.temperature.celsius.f32le.v1"

// encoding turns the payload of one SensorLink frame into ROS 2 messages.
type encoding struct {
	// ddsType is the type name on the wire; rosType is how ROS 2 tools show it.
	ddsType, rosType string
	// encode returns the serialized messages for one frame. stamp is when the
	// frame's first reading was taken and period the time between readings,
	// zero when the source does not say. It may return both messages and an
	// error when only part of the frame was usable.
	encode func(stamp time.Time, period time.Duration, payload []byte) ([][]byte, error)
}

// encodingFor picks the ROS 2 message for a sensor channel. A schema this
// agent knows becomes its standard message; anything else is published as the
// bytes the source sent.
func encodingFor(f *sensorlinkpb.SensorFormat, frameID string) encoding {
	if f.GetSchema() == schemaTemperature && f.GetSampleBytes() == 4 {
		return encoding{
			ddsType: "sensor_msgs::msg::dds_::Temperature_",
			rosType: "sensor_msgs/msg/Temperature",
			encode: func(stamp time.Time, period time.Duration, payload []byte) ([][]byte, error) {
				return encodeTemperature(stamp, period, frameID, payload)
			},
		}
	}
	schema := f.GetSchema()
	return encoding{
		ddsType: "std_msgs::msg::dds_::UInt8MultiArray_",
		rosType: "std_msgs/msg/UInt8MultiArray",
		encode: func(_ time.Time, _ time.Duration, payload []byte) ([][]byte, error) {
			return [][]byte{encodeBytes(schema, payload)}, nil
		},
	}
}

// encodeTemperature serializes one sensor_msgs/msg/Temperature per float32
// reading in payload. stamp is when the first reading was taken; each later
// one is stamped a period after the one before, as the source took them.
// Variance is 0, which ROS 2 defines as unknown.
func encodeTemperature(stamp time.Time, period time.Duration, frameID string, payload []byte) ([][]byte, error) {
	if len(payload) == 0 || len(payload)%4 != 0 {
		return nil, fmt.Errorf("temperature frame of %d bytes is not a whole number of 4-byte samples", len(payload))
	}
	var out [][]byte
	var bad error
	for off := 0; off < len(payload); off += 4 {
		celsius := float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[off:])))
		if math.IsNaN(celsius) || math.IsInf(celsius, 0) {
			bad = errors.New("temperature sample is not a finite number")
			continue
		}
		taken := stamp.Add(time.Duration(off/4) * period)
		e := cdr.NewEncoder()
		e.Int32(int32(taken.Unix())) // std_msgs/Header: stamp, then frame_id
		e.Uint32(uint32(taken.Nanosecond()))
		e.String(frameID)
		e.Float64(celsius)
		e.Float64(0)
		out = append(out, e.Payload())
	}
	return out, bad
}

// encodeBytes serializes a std_msgs/msg/UInt8MultiArray holding payload as
// one dimension. The dimension's label carries the channel's schema, so a
// subscriber can tell what the bytes are.
func encodeBytes(schema string, payload []byte) []byte {
	e := cdr.NewEncoder()
	e.Uint32(1) // layout.dim
	e.String(schema)
	e.Uint32(uint32(len(payload))) // size
	e.Uint32(uint32(len(payload))) // stride
	e.Uint32(0)                    // layout.data_offset
	e.Bytes(payload)
	return e.Payload()
}

// topicFor names the ROS 2 topic of a channel: /sensorlink/<source>/<channel>.
// The source is the name it was paired under, or its identity when it has
// none; the channel is the name the source gave it, or its number.
func topicFor(sourceName, sourceKey, channelName string, channelID uint32) string {
	return "/sensorlink/" + sourceToken(sourceName, sourceKey) + "/" + channelToken(channelName, channelID)
}

func sourceToken(name, key string) string {
	for _, candidate := range []string{name, key} {
		if token := nameToken(candidate); token != "" {
			return token
		}
	}
	return "source"
}

func channelToken(name string, id uint32) string {
	if token := nameToken(name); token != "" {
		return token
	}
	return fmt.Sprintf("channel_%d", id)
}

// maxToken bounds one token of a topic name. A ROS 2 topic name may be 255
// characters long, and a topic here has two tokens and a suffix.
const maxToken = 64

// nameToken turns free text into one valid token of a ROS 2 name: letters,
// digits and single underscores, not starting with a digit and not starting
// or ending with an underscore, cut to maxToken characters. It returns ""
// when nothing usable is left.
func nameToken(s string) string {
	var b strings.Builder
	pending := false
	for _, r := range s {
		// Room for this character, an underscore before it and a leading x.
		if b.Len()+3 > maxToken {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if pending && b.Len() > 0 {
				b.WriteByte('_')
			}
			pending = false
			b.WriteRune(r)
			continue
		}
		pending = true
	}
	token := b.String()
	if token != "" && token[0] >= '0' && token[0] <= '9' {
		return "x" + token
	}
	return token
}
