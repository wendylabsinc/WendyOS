package services

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

// inferenceSample is the camera sample identity the model worker carries back
// with the prediction for a picture.
func inferenceSample(s SensorSample) *inference.Sample {
	return &inference.Sample{ID: s.SampleID, UnixNanos: s.UnixNanos, BootNanos: s.BootNanos}
}
