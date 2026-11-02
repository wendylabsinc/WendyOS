package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLookCommandFailurePreservesCauseAndStops(t *testing.T) {
	for _, failedStop := range []bool{false, true} {
		r, f := readySiteRobot()
		f.moveError = status.Error(codes.Unavailable, "paired Go2 is not connected")
		if failedStop {
			f.stopError = context.DeadlineExceeded
		}
		var recorded []siteCommandFault
		r.commandFaultSink = func(f siteCommandFault) { recorded = append(recorded, f) }
		if err := r.Look(1, 1); err != nil {
			t.Fatal(err)
		}
		r.stepLocked(time.Now())
		if r.moving || len(f.commands) != 2 || f.commands[1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
			t.Fatal("failed move did not stop")
		}
		if len(recorded) < 1 || recorded[0].Command != pb.UnitreeCommand_UNITREE_COMMAND_MOVE || recorded[0].Code != "Unavailable" || recorded[0].Message != "paired Go2 is not connected" {
			t.Fatal("lost original failure", recorded)
		}
		if failedStop {
			if !r.stopUnconfirmed || len(recorded) != 2 || recorded[1].Command != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("lost recovery stop failure")
			}
		} else if !strings.Contains(r.message, "paired Go2 is not connected") {
			t.Fatal("UI discarded the cause", r.message)
		}
	}
}
func TestLookCommandTimeoutReportsTimeout(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, status.Error(codes.DeadlineExceeded, "context deadline exceeded")} {
		if got := siteCommandFailure("Look turn command", err); !strings.Contains(got, "timed out") {
			t.Fatal(got)
		}
	}
}
