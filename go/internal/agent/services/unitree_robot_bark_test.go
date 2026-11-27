package services

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type barkingFixtureLink struct{ *teleopFixtureLink }

func (l barkingFixtureLink) Bark(context.Context) error { l.record("bark"); return nil }

func TestUnitreeRobotBarkPlaysWithoutCancellingMoveExpiry(t *testing.T) {
	bark := pb.UnitreeCommand(unitree.CommandBark)
	c, l, pairing := teleopFixture(t, true, 80*time.Millisecond)
	if err := teleopSend(c, bark, 0, 0); status.Code(err) != codes.Unimplemented || len(l.log()) != 0 {
		t.Fatalf("a connection without a speaker: %v %v", err, l.log())
	}
	pairing.mu.Lock()
	pairing.sessions["GO2TEST1"].link = barkingFixtureLink{l}
	pairing.mu.Unlock()
	if err := teleopSend(c, pb.UnitreeCommand_UNITREE_COMMAND_MOVE, .5, 0); err != nil {
		t.Fatal(err)
	}
	if err := teleopSend(c, bark, 0, 0); err != nil {
		t.Fatal(err)
	}
	// The move was never refreshed, so its expiry must still stop the robot.
	deadline := time.Now().Add(2 * time.Second)
	for !sameCalls(l.log(), []string{"move", "bark", "stop"}) {
		if time.Now().After(deadline) {
			t.Fatalf("calls: %v", l.log())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
