package ros2sensor

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func f32le(values ...float32) []byte {
	var out []byte
	for _, v := range values {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(v))
	}
	return out
}

// decodedTemperature reads a sensor_msgs/msg/Temperature payload back.
type decodedTemperature struct {
	sec               int32
	nanosec           uint32
	frameID           string
	celsius, variance float64
}

func decodeTemperature(t *testing.T, payload []byte) decodedTemperature {
	t.Helper()
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out decodedTemperature
	out.sec, _ = d.Int32()
	out.nanosec, _ = d.Uint32()
	out.frameID, _ = d.String()
	celsius, _ := d.Uint64()
	variance, err := d.Uint64()
	if err != nil || d.Remaining() != 0 {
		t.Fatalf("payload is not one Temperature message: %v, %d bytes left", err, d.Remaining())
	}
	out.celsius, out.variance = math.Float64frombits(celsius), math.Float64frombits(variance)
	return out
}

func TestTemperatureChannelBecomesTemperatureMessages(t *testing.T) {
	enc := encodingFor(&sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4, RateHz: 1}, "sensorlink/porch")
	if enc.ddsType != "sensor_msgs::msg::dds_::Temperature_" || enc.rosType != "sensor_msgs/msg/Temperature" {
		t.Fatalf("type = %q (%q)", enc.ddsType, enc.rosType)
	}
	now := time.Unix(1700000000, 250)
	messages, err := enc.encode(now, time.Second, f32le(21.5))
	if err != nil || len(messages) != 1 {
		t.Fatalf("encode: %d messages, %v", len(messages), err)
	}
	// std_msgs/Header (stamp, frame_id), then float64 temperature and variance.
	want := []byte{0x00, 0x01, 0x00, 0x00}
	want = binary.LittleEndian.AppendUint32(want, 1700000000)
	want = binary.LittleEndian.AppendUint32(want, 250)
	want = binary.LittleEndian.AppendUint32(want, uint32(len("sensorlink/porch")+1))
	want = append(want, "sensorlink/porch\x00"...)
	for (len(want)-4)%8 != 0 {
		want = append(want, 0)
	}
	want = binary.LittleEndian.AppendUint64(want, math.Float64bits(21.5))
	want = binary.LittleEndian.AppendUint64(want, 0) // variance unknown
	if !bytes.Equal(messages[0], want) {
		t.Fatalf("message = % x\nwant      % x", messages[0], want)
	}
}

func TestOneFrameWithSeveralTemperatureSamplesIsPublishedInOrder(t *testing.T) {
	enc := encodingFor(&sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4}, "f")
	messages, err := enc.encode(time.Unix(5, 0), 0, f32le(20, -3.25, 40))
	if err != nil || len(messages) != 3 {
		t.Fatalf("encode: %d messages, %v", len(messages), err)
	}
	for i, want := range []float64{20, -3.25, 40} {
		if got := decodeTemperature(t, messages[i]); got.celsius != want || got.frameID != "f" || got.sec != 5 {
			t.Fatalf("message %d = %+v, want %v °C", i, got, want)
		}
	}
}

func TestReadingsBatchedInOneFrameAreStampedOnePeriodApart(t *testing.T) {
	enc := encodingFor(&sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4, RateHz: 50}, "f")
	// The frame's time is that of its first reading. The board took the others
	// one period later each, and a bad reading still takes up its period.
	first := time.Unix(5, 990_000_000)
	messages, _ := enc.encode(first, 20*time.Millisecond, f32le(20, float32(math.NaN()), 22))
	if len(messages) != 2 {
		t.Fatalf("%d messages, want the two good readings", len(messages))
	}
	for i, want := range []time.Time{first, first.Add(40 * time.Millisecond)} {
		got := decodeTemperature(t, messages[i])
		if stamp := time.Unix(int64(got.sec), int64(got.nanosec)); !stamp.Equal(want) {
			t.Fatalf("reading %d is stamped %s, want %s", i, stamp.UTC().Format(time.RFC3339Nano), want.UTC().Format(time.RFC3339Nano))
		}
	}
}

func TestMalformedTemperatureFramesAreRejected(t *testing.T) {
	enc := encodingFor(&sensorlinkpb.SensorFormat{Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 4}, "f")
	for name, payload := range map[string][]byte{
		"empty":             nil,
		"not whole samples": {1, 2, 3, 4, 5},
		"not a number":      f32le(float32(math.NaN())),
		"infinite":          f32le(float32(math.Inf(1))),
	} {
		if messages, err := enc.encode(time.Now(), 0, payload); err == nil || len(messages) != 0 {
			t.Errorf("%s: published %d messages, error %v", name, len(messages), err)
		}
	}
	// A bad sample does not take the good ones in the same frame with it.
	messages, err := enc.encode(time.Now(), 0, f32le(20, float32(math.NaN()), 22))
	if err == nil || len(messages) != 2 {
		t.Fatalf("frame with one bad sample: %d messages, error %v; want the two good ones and an error", len(messages), err)
	}
}

func TestOtherSensorChannelsBecomeRawByteMessages(t *testing.T) {
	for name, format := range map[string]*sensorlinkpb.SensorFormat{
		"schema this agent has no message for": {Schema: "wendy.imu.v1", SampleBytes: 24},
		"temperature in an unexpected width":   {Schema: "wendy.temperature.celsius.f32le.v1", SampleBytes: 8},
		"no schema declared":                   {},
	} {
		enc := encodingFor(format, "f")
		if enc.ddsType != "std_msgs::msg::dds_::UInt8MultiArray_" || enc.rosType != "std_msgs/msg/UInt8MultiArray" {
			t.Fatalf("%s: type = %q (%q)", name, enc.ddsType, enc.rosType)
		}
		messages, err := enc.encode(time.Now(), 0, []byte{9, 8, 7})
		if err != nil || len(messages) != 1 {
			t.Fatalf("%s: %d messages, %v", name, len(messages), err)
		}
		// layout.dim[0] {label, size, stride}, layout.data_offset, data.
		d, err := cdr.NewDecoder(messages[0])
		if err != nil {
			t.Fatal(err)
		}
		dims, _ := d.Uint32()
		label, _ := d.String()
		size, _ := d.Uint32()
		stride, _ := d.Uint32()
		offset, _ := d.Uint32()
		data, err := d.Bytes()
		if err != nil || d.Remaining() != 0 {
			t.Fatalf("%s: not one UInt8MultiArray: %v, %d bytes left", name, err, d.Remaining())
		}
		// The schema travels as the dimension label, so a subscriber can tell
		// what the bytes are without asking the agent.
		if dims != 1 || label != format.GetSchema() || size != 3 || stride != 3 || offset != 0 || !bytes.Equal(data, []byte{9, 8, 7}) {
			t.Fatalf("%s: dims=%d label=%q size=%d stride=%d offset=%d data=% x", name, dims, label, size, stride, offset, data)
		}
	}
}

func TestTopicNamesAreValidROS2Names(t *testing.T) {
	for _, tc := range []struct {
		name, key, channel string
		id                 uint32
		want               string
	}{
		{"porch-board", "asset:7", "temperature", 3, "/sensorlink/porch_board/temperature"},
		{"", "asset:7", "chip temp (°C)", 3, "/sensorlink/asset_7/chip_temp_C"},
		{"", "wendy://org/1/asset/7", "", 5, "/sensorlink/wendy_org_1_asset_7/channel_5"},
		// A token may not start with a digit, be empty, or repeat or end in underscores.
		{"3rd floor", "asset:7", "9dof", 1, "/sensorlink/x3rd_floor/x9dof"},
		{"__a__b__", "asset:7", "!!!", 2, "/sensorlink/a_b/channel_2"},
		{"///", "", "ok", 2, "/sensorlink/source/ok"},
	} {
		if got := topicFor(tc.name, tc.key, tc.channel, tc.id); got != tc.want {
			t.Errorf("topicFor(%q, %q, %q, %d) = %q, want %q", tc.name, tc.key, tc.channel, tc.id, got, tc.want)
		}
	}
}
