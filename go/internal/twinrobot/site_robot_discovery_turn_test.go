package twinrobot

import (
	"context"
	"google.golang.org/grpc"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func discoveryTurnFixture(direction float64, now time.Time) (*SiteRobot, *robotCommands) {
	r, commands := readySiteRobot()
	start := direction * 3.05
	target := start + direction*math.Pi/6
	r.observed.Pose.Yaw = start
	r.discoveryMove = &discoveryMove{origin: sitenav.Point{}, target: sitenav.Point{X: .4 * math.Cos(target), Y: .4 * math.Sin(target)}, yaw: target, turning: true, startYaw: start, turnDirection: direction, turnProgress: start, progressAt: now, epoch: r.poseEpoch}
	r.discoveryHistory = discoveryHistory{visited: []sitenav.Point{{}}}
	r.started, r.heartbeat = now, now
	r.discoveryState, r.moving = 1, true
	return r, commands
}

func TestDiscoveryTurnBrakesAndSettlesWithDelayedJitteredOdometry(t *testing.T) {
	for _, direction := range []float64{-1, 1} {
		for _, lag := range []time.Duration{0, 180 * time.Millisecond, 300 * time.Millisecond} {
			start := time.Now()
			r, commands := discoveryTurnFixture(direction, start)
			type sample struct {
				at  time.Duration
				yaw float64
			}
			samples := []sample{{at: -time.Second, yaw: r.observed.Pose.Yaw}, {yaw: r.observed.Pose.Yaw}}
			actualYaw := r.observed.Pose.Yaw
			var elapsed time.Duration
			velocity := 0.0
			stopped, advanced := false, false
			for tick := 0; tick < 90 && r.moving; tick++ {
				dt := []time.Duration{60 * time.Millisecond, 140 * time.Millisecond, 100 * time.Millisecond}[tick%3]
				elapsed += dt
				actualYaw += velocity * dt.Seconds()
				samples = append(samples, sample{elapsed, actualYaw})
				captured := elapsed - lag
				measured := samples[0].yaw
				for i := 1; i < len(samples); i++ {
					if samples[i].at >= captured {
						f := float64(captured-samples[i-1].at) / float64(samples[i].at-samples[i-1].at)
						measured = samples[i-1].yaw + f*(samples[i].yaw-samples[i-1].yaw)
						break
					}
				}
				now := start.Add(elapsed)
				r.heartbeat = now
				r.observed.Pose.Received = now
				r.observed.Pose.Captured = start.Add(captured)
				r.observed.CloudAt = now
				r.observed.Pose.Yaw = math.Remainder(measured, 2*math.Pi)
				before := len(commands.commands)
				r.stepLocked(now)
				if len(commands.commands) == before {
					continue
				}
				cmd := commands.commands[len(commands.commands)-1]
				if cmd.Command == pb.UnitreeCommand_UNITREE_COMMAND_STOP {
					velocity = 0
					stopped = true
					continue
				}
				if cmd.Forward != 0 {
					if !stopped || math.Abs(math.Remainder(actualYaw-r.discoveryMove.yaw, 2*math.Pi)) > .015 {
						t.Fatal("translated before measured settling")
					}
					advanced = true
					break
				}
				if math.Abs(cmd.Yaw) != lookSpeed || cmd.Yaw*direction <= 0 {
					t.Fatal("unsupported or reversed turn", cmd)
				}
				velocity = cmd.Yaw
			}
			if !advanced {
				t.Fatalf("direction%v lag%v failed: %s", direction, lag, r.message)
			}
		}
	}
}

func TestDiscoveryTurnRejectsLargeCrossingAndChecksSettledPath(t *testing.T) {
	for _, kind := range []string{"large-overshoot", "blocked-path", "stop-failed", "fresh-capture-required"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			r, commands := discoveryTurnFixture(1, now)
			move := r.discoveryMove
			r.observed.Pose.Yaw = math.Remainder(move.yaw+.06, 2*math.Pi)
			if kind == "large-overshoot" {
				r.observed.Pose.Yaw = math.Remainder(move.yaw+.11, 2*math.Pi)
			}
			if kind == "stop-failed" {
				commands.stopError = errLookTest{}
			}
			r.stepLocked(now)
			if kind == "large-overshoot" || kind == "stop-failed" {
				if r.moving || r.discoveryState != 3 {
					t.Fatal("unsafe turn continued")
				}
				if kind == "large-overshoot" && !strings.Contains(r.message, "0.100 rad") {
					t.Fatal(r.message)
				}
				return
			}
			if !move.turnStopping {
				t.Fatal("crossing did not brake")
			}
			if kind == "fresh-capture-required" {
				r.observed.Pose.Captured = now.Add(-time.Millisecond)
				r.stepLocked(now.Add(200 * time.Millisecond))
				if !move.turning || len(commands.commands) != 1 {
					t.Fatal("old pose authorized forward")
				}
				return
			}
			r.observed.Grid.Obstacles[sitenav.Cell{}] = true
			for _, dt := range []time.Duration{100 * time.Millisecond, 250 * time.Millisecond} {
				at := now.Add(dt)
				r.heartbeat = at
				r.observed.Pose.Captured = at
				r.observed.Pose.Received = at
				r.observed.CloudAt = at
				r.stepLocked(at)
			}
			if r.moving || r.discoveryState != 3 {
				t.Fatal("settled turn bypassed measured path clearance")
			}
			for _, cmd := range commands.commands {
				if cmd.Forward != 0 {
					t.Fatal("unsafe translation")
				}
			}
		})
	}
}

type delayedDiscoveryStop struct {
	*robotCommands
	acknowledged time.Time
}

func (c *delayedDiscoveryStop) Command(ctx context.Context, q *pb.UnitreeCommandRequest, options ...grpc.CallOption) (*pb.UnitreeCommandResponse, error) {
	if q.Command == pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		time.Sleep(20 * time.Millisecond)
		c.acknowledged = time.Now()
	}
	return c.robotCommands.Command(ctx, q, options...)
}
func TestDiscoveryTurnSettlingStartsAfterStopAcknowledgement(t *testing.T) {
	now := time.Now()
	r, commands := discoveryTurnFixture(1, now)
	delayed := &delayedDiscoveryStop{robotCommands: commands}
	r.client = delayed
	r.observed.Pose.Yaw = math.Remainder(r.discoveryMove.yaw, 2*math.Pi)
	r.stepLocked(now)
	move := r.discoveryMove
	if move.turnStopAt.Before(delayed.acknowledged) {
		t.Fatal("settling cutoff predates Stop acknowledgement")
	}
	r.observed.Pose.Captured = now.Add(time.Millisecond)
	r.observed.Pose.Received = time.Now()
	r.stepLocked(time.Now())
	if !move.turnSampleAt.IsZero() || !move.turning || len(commands.commands) != 1 {
		t.Fatal("pre-ack pose counted as settling evidence")
	}
}

func TestDiscoveryTurnWaitsForCoastingAndRejectsExcess(t *testing.T) {
	for _, coast := range []float64{lookSpeed * .1, .15} {
		now := time.Now()
		r, commands := discoveryTurnFixture(1, now)
		target := r.discoveryMove.yaw
		r.observed.Pose.Yaw = math.Remainder(target, 2*math.Pi)
		r.stepLocked(now)
		// A Stop acknowledgement does not instantaneously remove physical angular
		// velocity. Supply one further tick of coast before stable measurements.
		for i, offset := range []float64{coast / 2, coast, coast, coast} {
			at := now.Add(time.Duration(i+1) * 100 * time.Millisecond)
			r.heartbeat = at
			r.observed.Pose.Captured = at
			r.observed.Pose.Received = at
			r.observed.CloudAt = at
			r.observed.Pose.Yaw = math.Remainder(target+offset, 2*math.Pi)
			r.stepLocked(at)
			if !r.moving {
				break
			}
			for _, cmd := range commands.commands {
				if cmd.Forward != 0 && i < 3 {
					t.Fatal("translated while coasting")
				}
			}
		}
		if coast > .1 {
			if r.moving || r.discoveryState != 3 {
				t.Fatal("accepted excessive coast")
			}
			for _, cmd := range commands.commands {
				if cmd.Forward != 0 {
					t.Fatal("translated after excessive coast")
				}
			}
		} else if !r.moving || r.discoveryMove == nil || r.discoveryMove.turning {
			t.Fatal("did not revalidate a bounded settled turn", r.message)
		}
	}
}

func TestDiscoverySettledTurnPreservesRadiusAndViewSeparation(t *testing.T) {
	for _, kind := range []string{"radius", "separation"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			r, commands := discoveryTurnFixture(1, now)
			if kind == "radius" {
				r.discoveryHistory.visited = []sitenav.Point{{X: -1.31}}
			} else {
				r.discoveryHistory.visited = append(r.discoveryHistory.visited, r.discoveryMove.target)
			}
			r.observed.Pose.Yaw = math.Remainder(r.discoveryMove.yaw, 2*math.Pi)
			r.stepLocked(now)
			for _, dt := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond} {
				at := now.Add(dt)
				r.heartbeat = at
				r.observed.Pose.Captured = at
				r.observed.Pose.Received = at
				r.observed.CloudAt = at
				r.stepLocked(at)
			}
			if r.moving || r.discoveryState != 3 {
				t.Fatal("settled replan escaped", kind)
			}
			for _, cmd := range commands.commands {
				if cmd.Forward != 0 {
					t.Fatal("unsafe translation")
				}
			}
		})
	}
}
