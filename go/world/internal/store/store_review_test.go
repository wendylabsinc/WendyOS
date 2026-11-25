package store

import (
	"testing"
	"time"
)

func TestReviewRealClockCheckpointDoesNotRejectOwnExpiry(t *testing.T) {
	path, cfg, _, component := fixture(t)
	cfg.Now = time.Now
	component.Timing.SampleTime = time.Now()
	component.TTL = time.Minute
	s := open(t, path, cfg)
	apply(t, s, component)
	for i := 0; i < 20; i++ {
		if _, err := s.Snapshot(); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}
