package ros2sensor

import (
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// decodedTiming is a diagnostic_msgs/msg/DiagnosticArray with one status.
type decodedTiming struct {
	stamp                     time.Time
	frameID                   string
	level                     uint8
	name, message, hardwareID string
	keys                      []string
	values                    map[string]string
}

func decodeTiming(t *testing.T, payload []byte) decodedTiming {
	t.Helper()
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out decodedTiming
	sec, _ := d.Int32()
	nanosec, _ := d.Uint32()
	out.stamp = time.Unix(int64(sec), int64(nanosec))
	out.frameID, _ = d.String()
	if statuses, _ := d.Uint32(); statuses != 1 {
		t.Fatalf("%d statuses in the timing message, want 1", statuses)
	}
	out.level, _ = d.Uint8()
	out.name, _ = d.String()
	out.message, _ = d.String()
	out.hardwareID, _ = d.String()
	count, err := d.Uint32()
	if err != nil {
		t.Fatalf("timing message is cut short: %v", err)
	}
	out.values = map[string]string{}
	for i := uint32(0); i < count; i++ {
		key, _ := d.String()
		value, err := d.String()
		if err != nil {
			t.Fatalf("timing message is cut short at value %d: %v", i, err)
		}
		out.keys = append(out.keys, key)
		out.values[key] = value
	}
	if d.Remaining() != 0 {
		t.Fatalf("%d bytes left after the timing message", d.Remaining())
	}
	return out
}

func TestTimingOfAFramePlacedFromItsSourcesClock(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 123_000)
	got := decodeTiming(t, timing{
		stamp: stamp, sequence: 41, readings: 3, period: 20 * time.Millisecond,
		sourceUs: 6_000_250, placed: true, segment: 2, mapping: 1500 * time.Microsecond,
	}.encode("sensorlink/porch", "/sensorlink/porch/temperature", "asset:7"))

	if !got.stamp.Equal(stamp) || got.frameID != "sensorlink/porch" {
		t.Fatalf("header = %s %q", got.stamp, got.frameID)
	}
	if got.name != "/sensorlink/porch/temperature" || got.hardwareID != "asset:7" {
		t.Fatalf("status names %q on %q", got.name, got.hardwareID)
	}
	want := map[string]string{
		"sequence":                    "41",
		"readings":                    "3",
		"sample_period_nanos":         "20000000",
		"native_clock_domain":         "SENSORLINK_SOURCE",
		"native_timestamp_nanos":      "6000250000",
		"native_uncertainty_nanos":    "unreported",
		"mapping_segment":             "sensorlink-source-2",
		"mapping_uncertainty_nanos":   "1500000",
		"timestamp_uncertainty_nanos": "unbounded",
		"confidence":                  "unbounded",
	}
	if !reflect.DeepEqual(got.values, want) {
		t.Fatalf("values = %v\nwant     %v", got.values, want)
	}
	// The source says when, but not how well it knows: that is worth a
	// warning, since the stamp may be off by more than anyone can tell.
	if got.level != 1 || got.message != "the source reports no uncertainty for its sample time" {
		t.Fatalf("level %d, message %q", got.level, got.message)
	}
}

func TestTimingOfAFrameStampedOnArrival(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0)
	got := decodeTiming(t, timing{stamp: stamp, sequence: 7, readings: 1}.encode("f", "/t", "asset:7"))
	want := map[string]string{
		"sequence":                    "7",
		"readings":                    "1",
		"native_clock_domain":         "AGENT_RECEIPT",
		"mapping_segment":             "agent-receipt",
		"timestamp_uncertainty_nanos": "unbounded",
		"confidence":                  "unbounded",
	}
	if !reflect.DeepEqual(got.values, want) {
		t.Fatalf("values = %v\nwant     %v", got.values, want)
	}
	if got.level != 1 || got.message != "the source sent no sample time; stamped on arrival" {
		t.Fatalf("level %d, message %q", got.level, got.message)
	}
	// A time the source did send but that cannot be one is kept as sent.
	unusable := decodeTiming(t, timing{stamp: stamp, sourceUs: ^uint64(0)}.encode("f", "/t", "asset:7"))
	if unusable.values["native_timestamp_nanos"] != "18446744073709551615000" || unusable.values["native_clock_domain"] != "AGENT_RECEIPT" {
		t.Fatalf("values = %v", unusable.values)
	}
}
