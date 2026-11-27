package ros2sensor

import (
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

const (
	// timingSuffix names the topic that goes with a channel's readings.
	timingSuffix  = "/timing"
	timingDDSType = "diagnostic_msgs::msg::dds_::DiagnosticArray_"
	timingROSType = "diagnostic_msgs/msg/DiagnosticArray"

	// diagnostic_msgs/msg/DiagnosticStatus levels.
	levelOK   = 0
	levelWarn = 1
)

// timing is what is known about when the first reading of a frame was taken.
// It is published beside the readings, one message per frame, because no
// standard ROS 2 sensor message has room for it: a header holds a time, not
// how well that time is known or which clock it was read from.
//
// The value names follow what Wendy Data records for a sample: the time on
// this device's clock and its uncertainty, the time as the source's own clock
// gave it, and the segment of the mapping between the two clocks.
type timing struct {
	// stamp is the time of the first reading on this device's clock, the same
	// as in the header of the reading itself.
	stamp time.Time
	// sequence is the frame's number at its source.
	sequence uint32
	// readings is how many readings the frame holds, zero when not known.
	readings int
	// period is the time between two readings, zero when not known.
	period time.Duration
	// sourceUs is the time the source stamped the frame with, as it sent it;
	// zero when it sent none.
	sourceUs uint64
	// uncertaintyUs is how far sourceUs may be from the moment the reading
	// was really taken, as the source measured it; nil when it does not say.
	uncertaintyUs *uint32
	// placed says stamp is sourceUs moved to this device's clock. Otherwise
	// stamp is when the frame arrived.
	placed bool
	// segment numbers the mapping between the two clocks: it changes when the
	// source's clock restarts or jumps.
	segment int
	// mapping is how far stamp may be from the agent's best estimate of the
	// offset between the clocks. It does not cover the fastest transit time of
	// the link, which is not measured.
	mapping time.Duration
}

// encode serializes the timing as a diagnostic_msgs/msg/DiagnosticArray with
// one status. Values are strings, as that message defines them.
func (t timing) encode(frameID, topic, source string) []byte {
	type value struct{ key, value string }
	values := []value{{"sequence", strconv.FormatUint(uint64(t.sequence), 10)}}
	if t.readings > 0 {
		values = append(values, value{"readings", strconv.Itoa(t.readings)})
	}
	if t.period > 0 {
		values = append(values, value{"sample_period_nanos", strconv.FormatInt(int64(t.period), 10)})
	}
	// Microseconds to nanoseconds in text, so that no value overflows.
	sourceNanos := strconv.FormatUint(t.sourceUs, 10) + "000"
	// The stamp has a bound only when the source gave one for its own time:
	// that, plus what moving the time to this device's clock adds.
	level, message := uint8(levelWarn), ""
	bound, confidence := "unbounded", "unbounded"
	if t.placed {
		native := "unreported"
		message = "the source reports no uncertainty for its sample time"
		if t.uncertaintyUs != nil {
			reported := time.Duration(*t.uncertaintyUs) * time.Microsecond
			native = strconv.FormatInt(int64(reported), 10)
			bound, confidence = strconv.FormatInt(int64(reported+t.mapping), 10), "source_reported"
			level, message = levelOK, "the source reports its sample time and how well it knows it"
		}
		values = append(values,
			value{"native_clock_domain", "SENSORLINK_SOURCE"},
			value{"native_timestamp_nanos", sourceNanos},
			value{"native_uncertainty_nanos", native},
			value{"mapping_segment", "sensorlink-source-" + strconv.Itoa(t.segment)},
			value{"mapping_uncertainty_nanos", strconv.FormatInt(int64(t.mapping), 10)})
	} else {
		message = "the source sent no sample time; stamped on arrival"
		values = append(values, value{"native_clock_domain", "AGENT_RECEIPT"})
		if t.sourceUs != 0 {
			values = append(values, value{"native_timestamp_nanos", sourceNanos})
		}
		values = append(values, value{"mapping_segment", "agent-receipt"})
	}
	values = append(values,
		value{"timestamp_uncertainty_nanos", bound},
		value{"confidence", confidence})

	e := cdr.NewEncoder()
	e.Int32(int32(t.stamp.Unix())) // std_msgs/Header: stamp, then frame_id
	e.Uint32(uint32(t.stamp.Nanosecond()))
	e.String(frameID)
	e.Uint32(1) // status
	e.Uint8(level)
	e.String(topic) // name
	e.String(message)
	e.String(source) // hardware_id
	e.Uint32(uint32(len(values)))
	for _, v := range values {
		e.String(v.key)
		e.String(v.value)
	}
	return e.Payload()
}
