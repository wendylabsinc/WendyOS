package twinrobot

import (
	"testing"
	"time"
)

func TestMappingAndSearchStatusCannotRenewMotionLease(t *testing.T) {
	r, _ := readySiteRobot()
	expired := time.Now().Add(-time.Second)
	r.mu.Lock()
	r.heartbeat = expired
	r.mu.Unlock()
	_ = r.Status()
	r.MappingSample()
	r.mu.Lock()
	heartbeat := r.heartbeat
	r.mu.Unlock()
	if heartbeat != expired {
		t.Fatal("Linux background work renewed operator lease")
	}
	_ = r.Snapshot()
	r.mu.Lock()
	heartbeat = r.heartbeat
	r.mu.Unlock()
	if heartbeat == expired {
		t.Fatal("operator polling did not renew lease")
	}
}
func TestReleaseOperatorStopsWithoutClosingObservationStream(t *testing.T) {
	r, _ := readySiteRobot()
	r.ReleaseOperator()
	if r.ctx.Err() != nil {
		t.Fatal("operator release ended observation subscription")
	}
	if e := r.Destination(100, "room", 1, 1, true); e == nil {
		t.Fatal("released operator could move robot")
	}
}
