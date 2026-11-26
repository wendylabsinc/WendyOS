package sitenav

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestTrackingRecoversTranslationAndHeadingDrift(t *testing.T) {
	a, furniture := semanticFixture(t)
	m, err := NewLayeredRoom(a, furniture)
	if err != nil {
		t.Fatal(err)
	}
	position, yaw := Point{2.1, 3.2}, .7
	cloud := fixtureSurfaceCloud(t, a, position, yaw)
	prior := PoseCandidate{Position: Point{2.32, 3.04}, Rotation: .78}
	// Check accuracy independently of runner speed and race instrumentation.
	// The production caller still caps each tracking attempt at 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	fits, err := TrackSurfaces(ctx, m, RegistrationInput{Cloud: cloud}, prior)
	if err != nil {
		t.Fatal(err)
	}
	fit, ok := SelectTrackingFit(fits, prior)
	if !ok {
		t.Fatalf("tracking rejected room: %+v", fits[0])
	}
	if fit.Position.Distance(position) > .04 || math.Abs(fit.Rotation-yaw) > .015 {
		t.Fatalf("wrong correction: %+v", fit)
	}
	t.Log("tracking duration", time.Since(started))
}

func TestTrackingDiscardsResultsWhenContextEnds(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "expired"}[deadline], func(t *testing.T) {
			a, furniture := semanticFixture(t)
			m, err := NewLayeredRoom(a, furniture)
			if err != nil {
				t.Fatal(err)
			}
			cloud := fixtureSurfaceCloud(t, a, Point{2.1, 3.2}, .7)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			fits, err := TrackSurfaces(ctx, m, RegistrationInput{Cloud: cloud}, PoseCandidate{Position: Point{2.32, 3.04}, Rotation: .78})
			if !errors.Is(err, want) || len(fits) != 0 {
				t.Fatalf("ended context returned fits=%v, err=%v; want %v", fits, err, want)
			}
		})
	}
}

func TestAutomaticTrackingRejectsUncertainOrExcessiveCorrections(t *testing.T) {
	prior := PoseCandidate{Position: Point{2, 3}, Rotation: .3}
	valid := RegistrationFit{Pose: PoseCandidate{Position: Point{2.1, 3}, Rotation: .32, RMS: .04, Coverage: .85}, Valid: true, Plausible: true, Architecture: 80, Directions: 2, Cost: .005}
	for _, scenario := range []string{"valid", "parallel", "fragile", "furniture", "bad-fit", "outside-position", "outside-heading", "competing"} {
		t.Run(scenario, func(t *testing.T) {
			f := valid
			switch scenario {
			case "parallel":
				f.Directions = 1
			case "fragile":
				f.NeedsCorroboration = true
			case "furniture":
				f.Architecture = 0
				f.Furniture = 80
			case "bad-fit":
				f.Pose.RMS = .3
			case "outside-position":
				f.Pose.Position.X += .5
			case "outside-heading":
				f.Pose.Rotation += .3
			}
			fits := []RegistrationFit{f}
			if scenario == "competing" {
				other := f
				other.Pose.Position.X += .15
				other.Cost += .0001
				fits = append(fits, other)
			}
			_, ok := SelectTrackingFit(fits, prior)
			if ok != (scenario == "valid") {
				t.Fatalf("accepted=%v", ok)
			}
		})
	}
}
