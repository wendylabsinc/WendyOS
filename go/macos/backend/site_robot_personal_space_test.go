package backend

import (
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestPersonalSpaceChecksSegmentsNotOnlyEndpoints(t *testing.T) {
	center := sitenav.Point{}
	if personalSpaceClear(&center, []sitenav.Point{{X: -2}, {X: 2}}) {
		t.Fatal("segment crossing wearer accepted")
	}
	if !personalSpaceClear(&center, []sitenav.Point{{X: -2, Y: 2}, {X: 2, Y: 2}}) {
		t.Fatal("clear segment rejected")
	}
	if personalSpaceClear(&center, []sitenav.Point{{X: math.NaN()}}) {
		t.Fatal("invalid geometry accepted")
	}
}

func TestSummonPreviewAndMoveRetainWearerExclusion(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Summon(1, "site/revision", 2, 0, 3.5, 0, false); err != nil {
		t.Fatal(err)
	}
	if r.moving || len(f.commands) != 0 || r.personExclusion == nil {
		t.Fatal("preview moved or lost wearer protection")
	}
	if err := r.Summon(2, "site/revision", 2, 0, 3.5, 0, true); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	if !r.moving || len(f.commands) != 1 || f.commands[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
		t.Fatal("valid summon did not use the existing motion controller")
	}
	// A changed route may have clear endpoints but cross the wearer.
	r.route = []sitenav.Point{{X: 5}}
	r.stepLocked(time.Now())
	if r.moving || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("route through wearer did not stop")
	}
}

func TestSummonRejectsWrongClearanceAndStaleStopSequence(t *testing.T) {
	r, _ := readySiteRobot()
	if err := r.Summon(1, "site/revision", 2, 0, 2.5, 0, true); err == nil {
		t.Fatal("destination too close to wearer accepted")
	}
	r.Stop(3)
	if err := r.Summon(2, "site/revision", 2, 0, 3.5, 0, true); err == nil {
		t.Fatal("queued summon superseded Stop")
	}
}

func TestSummonWearerCoordinatesUseConfirmedAlignment(t *testing.T) {
	r, _ := readySiteRobot()
	r.alignment = sitenav.Alignment{Site: sitenav.Point{X: 10, Y: 20}, Rotation: math.Pi / 2}
	if err := r.Summon(1, "site/revision", 10, -22, 10, -23.5, false); err != nil {
		t.Fatal(err)
	}
	if r.personExclusion == nil || r.personExclusion.Distance(sitenav.Point{X: 3.5}) > 1e-6 {
		t.Fatal("wearer position was not transformed into robot coordinates", r.personExclusion)
	}
}

func TestSummonStopsBeforeBrakingHorizonEntersPersonalSpace(t *testing.T) {
	r, f := readySiteRobot()
	if err := r.Summon(1, "site/revision", 2, 0, 3.5, 0, true); err != nil {
		t.Fatal(err)
	}
	// The destination and segment are outside the exclusion circle, but the
	// robot's forward stopping horizon would enter it at this approach speed.
	r.observed.Pose.Position.X = 1.9
	r.stepLocked(time.Now())
	if r.moving || len(f.commands) == 0 || f.commands[len(f.commands)-1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
		t.Fatal("braking clearance did not stop the robot", f.commands)
	}
}

func TestOrdinaryDestinationClearsPreviousSummonExclusion(t *testing.T) {
	r, _ := readySiteRobot()
	if err := r.Summon(1, "site/revision", 2, 0, 3.5, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Destination(2, "site/revision", 2, 0, false); err != nil {
		t.Fatal(err)
	}
	if r.personExclusion != nil {
		t.Fatal("ordinary destination retained another order's wearer coordinates")
	}
}
