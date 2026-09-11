package services

import (
	"context"
	"errors"
)

// SensorSample is one sample handed from a producer to an in-agent consumer:
// the loopback pump that feeds a v4l2loopback node, or the episode capture
// adapter. It is agent-internal and has no wire form. The app-facing gRPC
// SensorService that once carried it was retired in favour of native reads,
// where an app opens the agent-fed node directly.
type SensorSample struct {
	SourceID string
	SampleID uint64
	// BootNanos is the agent's bracketed CLOCK_BOOTTIME receipt of the sample;
	// UncertaintyNanos is the bracket half-width.
	BootNanos        int64
	UncertaintyNanos int64
	Payload          []byte
	Encoding         string
	// SelfContained reports that Payload holds exactly one whole encoded unit.
	SelfContained bool
	// DroppedBefore counts samples lost between the producer and this
	// subscriber since the previous delivered sample.
	DroppedBefore uint64
	// SampleRateHz, Channels and DurationNanos describe a Payload that carries
	// a BUFFER of equally spaced samples rather than a single instant. When
	// SampleRateHz is non-zero, BootNanos names the FIRST sample in the
	// payload, and the k-th sample (zero-based, counting frames of Channels
	// interleaved values) lies at
	// BootNanos + k * 1000000000 / SampleRateHz. DurationNanos is the span the
	// whole payload covers, derived from its length. A consumer must count
	// samples into the buffer rather than assume a fixed chunk size, because a
	// producer may deliver buffers of varying length. All three are zero for a
	// single-instant sample, such as a camera frame, which is the compatible
	// default.
	SampleRateHz  uint32
	Channels      uint32
	DurationNanos int64
}

// sensorSubscription is one consumer attached to a producer.
type sensorSubscription interface {
	// Next blocks until the next sample is available. It returns a non-nil
	// error when the producer stopped or ctx was cancelled.
	Next(ctx context.Context) (SensorSample, error)
	// Close releases the subscription.
	Close()
}

// errSensorProducerStopped is returned by a subscription whose producer ended.
var errSensorProducerStopped = errors.New("sensor producer stopped")
