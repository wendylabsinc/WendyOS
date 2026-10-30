package backend

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type endedSiteObservationStream struct{}

func (endedSiteObservationStream) Recv() (*pb.UnitreeStreamMessage, error) {
	return nil, io.EOF
}

func TestSiteRobotStreamDisconnectStopsAndSurvivesSnapshot(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop-confirmed", true: "stop-failed"}[stopFails], func(t *testing.T) {
			r, commands := readySiteRobot()
			r.ctx, r.cancel = context.WithCancel(context.Background())
			defer r.cancel()
			r.moving = true
			r.lookTurn = &siteLookTurn{}
			r.lookState = 1
			r.route = []sitenav.Point{{X: 1}}
			r.preview = []sitenav.Point{{X: 2}}
			if stopFails {
				commands.stopError = errors.New("offline")
			}
			r.receiveObservations(endedSiteObservationStream{})
			if r.ctx.Err() == nil || !r.closed || r.moving || r.frame != "" || r.lookTurn != nil || len(r.route)+len(r.preview) != 0 {
				t.Fatal("disconnect retained active state")
			}
			if len(commands.commands) != 1 || commands.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("disconnect did not issue only Stop", commands.commands)
			}
			for _, stale := range []bool{false, true} {
				if stale {
					r.observed.Pose.Received = time.Now().Add(-time.Minute)
				}
				s := r.Snapshot()
				if !s.Disconnected || s.Pose || s.Ready || s.Aligned || s.Moving || s.ScanReady || s.MeasurementsReady || len(s.Joints)+len(s.Route) != 0 {
					t.Fatal("terminal snapshot exposes usable observations", s)
				}
				if stopFails {
					if s.ControlWarning == "" || !strings.Contains(s.Message, "Stop was not acknowledged") {
						t.Fatal("disconnect hid unconfirmed Stop", s)
					}
				} else if !strings.Contains(s.Message, "stream disconnected") {
					t.Fatal("readiness hid stream failure", s.Message)
				}
			}
		})
	}
}

func TestSiteRobotDeliberateCloseDoesNotRequestReconnect(t *testing.T) {
	r, commands := readySiteRobot()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.cancel()
	r.receiveObservations(endedSiteObservationStream{})
	s := r.Snapshot()
	if s.Disconnected || s.Pose || s.Aligned || s.Ready || s.Message != "Go2 connection closed." || len(commands.commands) != 0 {
		t.Fatal("deliberate replacement would reopen or expose stale state", s)
	}
}

func TestSiteRobotSilentStreamTimesOutWithoutWeakeningFreshness(t *testing.T) {
	r, commands := readySiteRobot()
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer r.cancel()
	now := time.Now()
	r.lastPosePacket = now
	r.checkObservationStreamLocked(now.Add(4 * time.Second))
	if r.closed {
		t.Fatal("subscription timed out before startup allowance")
	}
	// LiDAR/joints cannot keep a subscription with no odometry alive.
	r.lastObservationLog = now.Add(time.Hour)
	r.receiveObservationLocked(sitenav.JointTopic, nil, false, now, now.Add(4*time.Second))
	r.checkObservationStreamLocked(now.Add(6 * time.Second))
	if !r.Snapshot().Disconnected || len(commands.commands) != 0 {
		t.Fatal("silent stream was not invalidated, or issued motion")
	}
	// Even a late packet cannot revive a cancelled handle.
	r.receiveObservationLocked(sitenav.PoseTopic, nil, false, now, now.Add(7*time.Second))
	if r.lastPosePacket != now {
		t.Fatal("late packet revived closed subscription")
	}
}
