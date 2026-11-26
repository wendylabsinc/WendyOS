package twinrobot

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func passiveLocalizationFixture(symmetric bool) (SiteFusionRequest, sitenav.SurfaceCloud) {
	walls := [][4]float64{{0, 0, 6, 0}, {6, 0, 6, 8}, {6, 8, 0, 8}, {0, 8, 0, 0}}
	if !symmetric {
		walls = append(walls, [4]float64{4, 4, 6, 4}, [4]float64{4, 4, 4, 6}, [4]float64{0, 1, 1, 2})
	}
	triangles := []sitenav.Triangle{{{X: 0, Y: 0, Z: 0}, {X: 6, Y: 0, Z: 0}, {X: 6, Y: 0, Z: -8}}, {{X: 0, Y: 0, Z: 0}, {X: 6, Y: 0, Z: -8}, {X: 0, Y: 0, Z: -8}}}
	var points []sitenav.Vertex
	position := sitenav.Point{X: 2.1, Y: 3.2}
	angle := .7
	s, c := math.Sincos(-angle)
	for _, w := range walls {
		a, b, topB, topA := sitenav.Vertex{X: w[0], Z: -w[1]}, sitenav.Vertex{X: w[2], Z: -w[3]}, sitenav.Vertex{X: w[2], Y: 2, Z: -w[3]}, sitenav.Vertex{X: w[0], Y: 2, Z: -w[1]}
		triangles = append(triangles, sitenav.Triangle{a, b, topB}, sitenav.Triangle{a, topB, topA})
		n := int(math.Ceil(math.Hypot(w[2]-w[0], w[3]-w[1]) / .1))
		for i := 0; i <= n; i++ {
			x, y := w[0]+(w[2]-w[0])*float64(i)/float64(n)-position.X, w[1]+(w[3]-w[1])*float64(i)/float64(n)-position.Y
			for _, height := range []float64{.15, .2, .25, .45, .5, .55} {
				points = append(points, sitenav.Vertex{X: c*x - s*y, Y: s*x + c*y, Z: height})
			}
		}
	}
	input := SiteFusionRequest{Frame: "room/revision"}
	for _, triangle := range triangles {
		for _, p := range triangle {
			input.Architecture = append(input.Architecture, p.X, p.Y, p.Z)
		}
	}
	return input, sitenav.ExtractSurfaces(points, 0)
}

func TestPassiveLocalizationConfirmsMeasuredUniquePoseWithoutCommands(t *testing.T) {
	input, cloud := passiveLocalizationFixture(false)
	r, commands := readySiteRobot()
	r.frame = ""
	r.commandSequence = 7000000
	r.observed.Surfaces = cloud
	r.observed.Grid = sitenav.Grid{}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				r.mu.Lock()
				r.observed.Pose.Received = now
				r.observed.Pose.Captured = now
				r.observed.CloudAt = now
				r.mu.Unlock()
			}
		}
	}()
	result := r.LocalizeCommand(ctx, input)
	if result.State != "completed" || !result.Snapshot.Aligned || result.Snapshot.Frame != input.Frame || math.Abs(result.Snapshot.X-2.1) > .15 || math.Abs(result.Snapshot.Z+3.2) > .15 {
		t.Fatalf("%+v", result)
	}
	r.mu.Lock()
	sequence := r.commandSequence
	r.mu.Unlock()
	if sequence != 7000002 || len(commands.commands) != 0 {
		t.Fatalf("sequence%d sent%d robot commands", sequence, len(commands.commands))
	}
}

func TestPassiveLocalizationDoesNotConfirmAmbiguousRoom(t *testing.T) {
	input, cloud := passiveLocalizationFixture(true)
	r, commands := readySiteRobot()
	r.frame = ""
	r.observed.Surfaces = cloud
	r.observed.Grid = sitenav.Grid{}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				r.mu.Lock()
				r.observed.Pose.Received = now
				r.observed.Pose.Captured = now
				r.observed.CloudAt = now
				r.mu.Unlock()
			}
		}
	}()
	result := r.LocalizeCommand(ctx, input)
	if result.State != "rejected" || result.Snapshot.Aligned || !strings.Contains(result.Message, "Several room positions") || len(commands.commands) != 0 {
		t.Fatalf("%+v commands%d", result, len(commands.commands))
	}
}

func TestPassiveLocalizationRejectsMotionPendingAndCancellation(t *testing.T) {
	input, _ := passiveLocalizationFixture(false)
	for _, fault := range []string{"moving", "chat", "turn", "forward", "stop-unconfirmed", "canceled", "stale"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = ""
			r.commandSequence = 100
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "moving":
				r.moving = true
			case "chat":
				r.intentActive = true
			case "turn":
				r.lookTurn = &siteLookTurn{}
			case "forward":
				r.discoveryMove = &discoveryMove{}
			case "stop-unconfirmed":
				r.stopUnconfirmed = true
			case "canceled":
				cancel()
			case "stale":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			}
			result := r.LocalizeCommand(ctx, input)
			if result.State == "completed" || len(commands.commands) != 0 || r.frame != "" {
				t.Fatalf("%+v commands%d", result, len(commands.commands))
			}
			if fault != "stale" && r.commandSequence != 100 {
				t.Fatal("rejected passive request consumed operator sequence")
			}
		})
	}
}

func TestPassiveLocalizationConfirmationRejectsNewerIntentOrCanceledContext(t *testing.T) {
	for _, fault := range []string{"generation", "sequence", "canceled", "moving", "pending"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = ""
			r.commandSequence = 11
			r.intentGeneration = 3
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			passive := &passiveLocalizationRequest{ctx: ctx, sequence: 11, generation: 3}
			switch fault {
			case "generation":
				r.intentGeneration++
			case "sequence":
				r.commandSequence++
			case "canceled":
				cancel()
			case "moving":
				r.moving = true
			case "pending":
				r.lookTurn = &siteLookTurn{}
			}
			r.mu.Lock()
			err := r.confirmPassiveLocalizationLocked(passive, "room/revision")
			r.mu.Unlock()
			if err == nil || r.frame != "" || len(commands.commands) != 0 {
				t.Fatalf("err%v commands%d", err, len(commands.commands))
			}
		})
	}
}

func TestPassiveConfirmationAllowsBoundedIdlePoseWithoutRelaxingMotion(t *testing.T) {
	_, cloud := passiveLocalizationFixture(false)
	for _, mode := range []string{"idle", "too-old-pose", "old-cloud", "future-pose", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = ""
			r.commandSequence = 11
			r.observed.Surfaces = cloud
			now := time.Now()
			stamp := now.Add(-time.Second)
			r.observed.Pose.Received = stamp
			r.observed.Pose.Captured = stamp
			r.observed.CloudAt = now
			switch mode {
			case "too-old-pose":
				r.observed.Pose.Received = now.Add(-2100 * time.Millisecond)
				r.observed.Pose.Captured = r.observed.Pose.Received
			case "old-cloud":
				r.observed.CloudAt = now.Add(-time.Second)
			case "future-pose":
				r.observed.Pose.Captured = now.Add(time.Second)
			case "epoch":
				r.poseEpoch++
			}
			pose := r.observed.Pose
			r.localizationSequence = 11
			r.localizations = []siteRobotLocalization{{frame: "room/revision", pose: pose, created: now, surfaceBased: true, alignment: sitenav.Alignment{Site: sitenav.Point{X: 2, Y: 3}}}}
			passive := &passiveLocalizationRequest{ctx: context.Background(), sequence: 11}
			r.mu.Lock()
			err := r.confirmPassiveLocalizationLocked(passive, "room/revision")
			r.mu.Unlock()
			if mode == "idle" {
				if err != nil || r.frame != "room/revision" {
					t.Fatalf("idle confirmation: %v", err)
				}
				if r.observed.MeasurementsReady(time.Now()) == nil {
					t.Fatal("idle localization relaxed motion freshness")
				}
				result := r.ExecuteCommand(context.Background(), Command{Action: "turn", Degrees: 45})
				if result.State != "rejected" {
					t.Fatalf("stale idle pose authorized motion: %+v", result)
				}
			} else if err == nil || r.frame != "" {
				t.Fatalf("accepted %s: %v", mode, err)
			}
			if r.observed.Pose.Received != pose.Received || r.observed.Pose.Captured != pose.Captured || len(commands.commands) != 0 {
				t.Fatal("confirmation rewrote capture evidence or sent robot commands")
			}
		})
	}
}
