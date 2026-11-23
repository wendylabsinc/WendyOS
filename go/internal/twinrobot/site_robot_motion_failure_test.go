package twinrobot

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// Deliver a real, fresh compressed floor scan after the watchdog aborts.
func deliverHealthyMotionTestCloud(t *testing.T, r *SiteRobot) {
	t.Helper()
	now := time.Now()
	r.heartbeat = now
	r.observed.Pose.Received, r.observed.Pose.Captured = now, now
	raw := make([]byte, 2048)
	for i := range raw {
		raw[i] = 255
	}
	compressed := make([]byte, lz4.CompressBlockBound(len(raw)))
	n, err := lz4.CompressBlock(raw, compressed, nil)
	if err != nil || n == 0 {
		t.Fatal("compressing scan", err)
	}
	header, err := json.Marshal(map[string]any{"topic": sitenav.CloudTopic, "data": map[string]any{
		"src_size": len(raw), "origin": []float64{-2, -2, 0}, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9,
	}})
	if err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 12)
	binary.LittleEndian.PutUint16(packet, 2)
	binary.LittleEndian.PutUint32(packet[4:], uint32(len(header)))
	packet = append(append(packet, header...), compressed[:n]...)
	r.lastNavigationLog = now
	r.receiveObservationLocked(sitenav.CloudTopic, packet, true, now, now)
	if err := r.observed.Ready(now); err != nil {
		t.Fatal("fresh scan fixture rejected", err)
	}
}

func TestAbortedMotionReasonSurvivesTelemetryAndSearchCleanup(t *testing.T) {
	for _, kind := range []string{"look", "discovery"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := readySiteRobot()
			r.frame = "" // Current localization-readiness text must not hide an abort either.
			start := func(sequence uint64) error {
				if kind == "look" {
					return r.Look(sequence, 1)
				}
				return r.DiscoveryStep(sequence, 1)
			}
			if err := start(1); err != nil {
				t.Fatal(err)
			}
			r.heartbeat = time.Now().Add(-time.Second)
			r.stepLocked(time.Now())
			cause := r.message
			if !strings.Contains(cause, "map is no longer active") {
				t.Fatalf("wrong fault fixture: %q", cause)
			}
			deliverHealthyMotionTestCloud(t, r)
			for _, cleanup := range []bool{false, true} {
				if cleanup {
					r.Stop(2)
				}
				s := r.Status()
				if s.Message != cause || s.Moving || kind == "look" && s.LookState != 3 || kind == "discovery" && s.DiscoveryState != 3 {
					t.Fatalf("telemetry or cleanup erased terminal failure: %+v", s)
				}
			}
			if err := start(1); err == nil || r.motionFailure != cause {
				t.Fatal("rejected order acknowledged the failure")
			}
			if err := start(3); err != nil {
				t.Fatal("explicit retry failed", err)
			}
			if r.motionFailure != "" || r.Status().Message == cause || r.lookState == 3 || r.discoveryState == 3 {
				t.Fatal("new accepted attempt retained the previous failure")
			}
		})
	}
}

func TestMotionCompletionWithoutStopAcknowledgementIsAborted(t *testing.T) {
	for _, kind := range []string{"look", "discovery"} {
		t.Run(kind, func(t *testing.T) {
			r, commands := readySiteRobot()
			if kind == "look" {
				if err := r.Look(1, 1); err != nil {
					t.Fatal(err)
				}
				r.observed.Pose.Yaw = lookAngle
			} else {
				if err := r.DiscoveryStep(1, 1); err != nil {
					t.Fatal(err)
				}
				r.observed.Pose.Position.X = discoveryDistance
			}
			commands.stopError = context.DeadlineExceeded
			r.stepLocked(time.Now())
			deliverHealthyMotionTestCloud(t, r)
			s := r.Status()
			if s.Message != stopUnconfirmedMessage || !r.stopUnconfirmed || s.Ready || s.MeasurementsReady ||
				kind == "look" && s.LookState != 3 || kind == "discovery" && s.DiscoveryState != 3 {
				t.Fatalf("failed final Stop was reported as completion: %+v", s)
			}
		})
	}
}

func TestPassiveLocalizationWaitAndFailurePreserveMotionFailure(t *testing.T) {
	r, _ := readySiteRobot()
	r.lookState, r.discoveryState = 3, 3
	r.motionFailure, r.message = "prior discovery failure", "prior discovery failure"
	r.observed.CloudAt = time.Now().Add(-time.Second)
	report, err := r.FuseLocalization(1, SiteFusionRequest{Frame: "site/revision"})
	if err != nil || !report.WaitingForObservations || r.motionFailure != "prior discovery failure" || r.lookState != 3 || r.discoveryState != 3 {
		t.Fatalf("passive wait erased motion failure: report=%+v err=%v", report, err)
	}
	now := time.Now()
	r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now
	if _, err := r.FuseLocalization(2, SiteFusionRequest{Frame: "site/revision"}); err == nil || r.motionFailure != "prior discovery failure" {
		t.Fatal("failed passive match erased motion failure", err)
	}
}

func TestMotionRecoveryRequiresTemporaryLossAndAcknowledgedStop(t *testing.T) {
	for _, kind := range []string{"look", "discovery"} {
		for _, cause := range []string{"stale scan", "missing floor", "clearance", "stop rejected", "drift", "no progress", "move rejected", "operator lease", "closed"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				r, commands := readySiteRobot()
				if kind == "look" {
					if err := r.Look(1, 1); err != nil {
						t.Fatal(err)
					}
				} else if err := r.DiscoveryStep(1, 1); err != nil {
					t.Fatal(err)
				}
				switch cause {
				case "stale scan", "stop rejected":
					r.observed.CloudAt = time.Now().Add(-time.Second)
					if cause == "stop rejected" {
						commands.stopError = context.DeadlineExceeded
					}
				case "missing floor":
					r.observed.FloorFault = "No observed floor"
				case "clearance":
					r.observed.Grid.Floor = nil
				case "drift":
					r.observed.Pose.Position.Y += .2
				case "no progress":
					if kind == "look" {
						r.lookTurn.progressAt = time.Now().Add(-3 * time.Second)
					} else {
						r.discoveryMove.progressAt = time.Now().Add(-3 * time.Second)
					}
				case "move rejected":
					commands.moveError = context.DeadlineExceeded
				case "operator lease":
					r.heartbeat = time.Now().Add(-time.Second)
				case "closed":
					r.closed = true
				}
				r.stepLocked(time.Now())
				s := r.Status()
				wantRecovery := cause == "stale scan" || cause == "missing floor" || cause == "clearance"
				if s.Moving || s.MotionRecoveryAllowed != wantRecovery {
					t.Fatalf("wrong recovery classification: %+v", s)
				}
				if !wantRecovery {
					return
				}
				failure := s.Message
				deliverHealthyMotionTestCloud(t, r)
				s = r.Status()
				if !s.MotionRecoveryAllowed || s.Message != failure || s.ScanReceivedUnixNanos != r.observed.CloudAt.UnixNano() {
					t.Fatalf("fresh telemetry erased recovery cause/timestamp: %+v", s)
				}
				// A stopped search may not rearm just because telemetry resumes.
				r.Stop(2)
				if r.Status().MotionRecoveryAllowed {
					t.Fatal("explicit Stop retained automatic motion recovery")
				}
			})
		}
	}
}

func TestMotionObservationPreflightIsTypedAndCannotBypassLease(t *testing.T) {
	for _, kind := range []string{"look", "discovery"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := readySiteRobot()
			start := func(sequence uint64) error {
				if kind == "look" {
					return r.Look(sequence, 1)
				}
				return r.DiscoveryStep(sequence, 1)
			}
			r.observed.CloudAt = time.Now().Add(-time.Second)
			if err := start(1); !errors.Is(err, ErrObservationUnavailable) {
				t.Fatalf("missing observation classification: %v", err)
			}
			if r.Status().MotionRecoveryAllowed {
				t.Fatal("unstarted motion exposed an aborted-motion recovery flag")
			}
			r.heartbeat = time.Now().Add(-time.Second)
			if err := start(2); err == nil || errors.Is(err, ErrObservationUnavailable) {
				t.Fatalf("operator lease must end automatic recovery: %v", err)
			}
		})
	}
}

func TestMotionRecoveryLeaseCannotBeRearmedByPolling(t *testing.T) {
	r, _ := readySiteRobot()
	if err := r.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	r.observed.CloudAt = time.Now().Add(-time.Second)
	r.stepLocked(time.Now())
	if !r.Status().MotionRecoveryAllowed {
		t.Fatal("fixture did not abort on observations")
	}
	r.heartbeat = time.Now().Add(-time.Second)
	if r.Snapshot().MotionRecoveryAllowed {
		t.Fatal("renewed polling rearmed an expired recovery lease")
	}
}

func TestLookPreflightClearanceIsTypedAndDoesNotMove(t *testing.T) {
	r, commands := readySiteRobot()
	r.observed.Grid.Floor = nil
	if err := r.Look(1, 1); !errors.Is(err, ErrDiscoveryBlocked) {
		t.Fatalf("missing clearance classification: %v", err)
	}
	if r.moving || len(commands.commands) != 0 {
		t.Fatal("blocked preflight issued a movement command")
	}
}
