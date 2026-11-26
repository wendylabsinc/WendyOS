package twinrobot

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func objectFixture(t *testing.T) (*SiteRobot, *ObjectView, [4]float64) {
	t.Helper()
	r, _ := readySiteRobot()
	r.observed.Surfaces.Rejected = []sitenav.Vertex{{X: 2, Y: -.1, Z: .4}, {X: 2, Y: 0, Z: .55}, {X: 2, Y: .1, Z: .7}}
	view, err := r.BeginObjectView(time.Now().UnixNano(), 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	return r, view, [4]float64{.35, .1, .3, .8}
}

func TestObjectProjectionUsesMeasuredDepthAndRejectsBackground(t *testing.T) {
	r, view, box := objectFixture(t)
	target, err := r.LocateObjectView(view, box)
	if err != nil || math.Abs(target.X-2) > .01 || math.Abs(target.ApproachX-1.1) > .01 || target.Support != 3 || !strings.Contains(target.Calibration, "community-estimated") {
		t.Fatalf("%+v %v", target, err)
	}
	if _, _, err := projectedObject(nil, view.pose, box); err == nil {
		t.Fatal("invented range without measurements")
	}
	points := append(append([]sitenav.Vertex(nil), view.samples...), sitenav.Vertex{X: 4, Y: 0, Z: .6})
	if _, _, err := projectedObject(points, view.pose, box); err == nil {
		t.Fatal("accepted two depths")
	}
	points = append(append([]sitenav.Vertex(nil), view.samples...), sitenav.Vertex{X: 2, Y: .45, Z: .5})
	if _, _, err := projectedObject(points, view.pose, box); err == nil {
		t.Fatal("accepted background extending outside object")
	}
	if _, _, err := projectedObject(view.samples, view.pose, [4]float64{math.NaN(), 0, 1, 1}); err == nil {
		t.Fatal("accepted NaN box")
	}
}

func TestObjectViewRejectsChangedEvidence(t *testing.T) {
	for _, kind := range []string{"expired", "command", "generation", "alignment", "pose", "posture", "tilt", "stale", "object", "unknown-floor"} {
		t.Run(kind, func(t *testing.T) {
			r, view, box := objectFixture(t)
			switch kind {
			case "expired":
				view.at = time.Now().Add(-3 * time.Minute)
			case "command":
				r.commandSequence++
			case "generation":
				r.intentGeneration++
			case "alignment":
				r.alignment.Rotation = .1
			case "pose":
				r.observed.Pose.Position.X = .1
			case "posture":
				r.observed.Pose.Height += .05
			case "tilt":
				r.observed.Pose.Pitch = .1
			case "stale":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "object":
				r.observed.Surfaces.Rejected = nil
			case "unknown-floor":
				r.observed.Grid.Floor = map[sitenav.Cell]bool{}
				r.allowUnobservedFloor = true
				r.underBodyAllowance = true
			}
			if _, err := r.LocateObjectView(view, box); err == nil {
				t.Fatal("accepted changed evidence")
			}
		})
	}
}

func TestObjectViewClockModeAndPosePairing(t *testing.T) {
	r, _, _ := objectFixture(t)
	r.observed.ClockOffset = 3 * time.Second
	if _, err := r.BeginObjectView(time.Now().Add(3*time.Second).UnixNano(), 1280, 720); err != nil {
		t.Fatalf("gateway offset conversion: %v", err)
	}
	if _, err := r.BeginObjectView(time.Now().Add(3*time.Second).UnixNano(), 640, 480); err == nil {
		t.Fatal("accepted wrong mode")
	}
	r.observed.ClockOffset = 0
	r.observed.Pose.Received = time.Now().Add(-500 * time.Millisecond)
	if _, err := r.BeginObjectView(time.Now().UnixNano(), 1280, 720); err == nil {
		t.Fatal("accepted unmatched pose")
	}
}

func TestObjectApproachRejectsStaleAtAtomicDispatch(t *testing.T) {
	r, view, box := objectFixture(t)
	r.commandSequence++
	result := r.ExecuteCommand(context.Background(), Command{Action: "approach_object", ObjectView: view, ObjectBox: box})
	if result.State != "rejected" || r.intentActive || r.moving {
		t.Fatalf("%+v", result)
	}
}

func TestObjectApproachCompletesThroughMeasuredController(t *testing.T) {
	r, view, box := objectFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan CommandResult, 1)
	go func() {
		result <- r.ExecuteCommand(ctx, Command{Action: "approach_object", ObjectView: view, ObjectBox: box})
	}()
	for {
		select {
		case got := <-result:
			if got.State != "completed" || got.Snapshot.Moving {
				t.Fatalf("%+v", got)
			}
			return
		case <-time.After(5 * time.Millisecond):
			r.mu.Lock()
			now := time.Now()
			r.observed.Pose.Received = now
			r.observed.Pose.Captured = now
			r.observed.CloudAt = now
			if r.moving {
				r.observed.Pose.Position = r.goal
				r.stepLocked(now)
			}
			r.mu.Unlock()
		}
	}
}
