package twinrobot

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestLocalizationSearchOwnerPreservesDesktopSequenceAndNewerMovement(t *testing.T) {
	r, commands := readySiteRobot()
	r.commandSequence = 7000000
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	if r.commandSequence != 7000000 {
		t.Fatal("internal search advanced desktop sequence")
	}
	r.Stop(7000001)
	if err := r.Look(7000002, -1); err != nil {
		t.Fatal("next desktop order was blocked by internal sequence", err)
	}
	count := len(commands.commands)
	heartbeat := r.heartbeat
	if status := owner.Status(); !status.Disconnected {
		t.Fatal("superseded search did not become terminal")
	}
	if r.heartbeat != heartbeat {
		t.Fatal("old owner renewed the new operator's lease")
	}
	owner.Stop(math.MaxUint64)
	owner.Close()
	if len(commands.commands) != count || !r.moving || r.commandSequence != 7000002 || r.intentActive {
		t.Fatal("old cleanup stopped or poisoned the new desktop command")
	}
	if err := owner.Look(2, 1); err == nil {
		t.Fatal("closed search started another turn")
	}
}

func TestLocalizationSearchOwnerCancellationStopsWithoutPolling(t *testing.T) {
	r, commands := readySiteRobot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := r.BeginLocalizationSearch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	r.stepLocked(time.Now())
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		closed, moving, active := owner.closed, r.moving, r.intentActive
		count := len(commands.commands)
		last := commands.commands[count-1].Command
		r.mu.Unlock()
		if closed {
			if moving || active || count != 2 || last != pb.UnitreeCommand_UNITREE_COMMAND_STOP {
				t.Fatal("cancel did not stop exactly the owned movement")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled owner did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(owner.Check(), context.Canceled) {
		t.Fatal("canceled search still passed ownership check")
	}
}

func TestLocalizationSearchOwnerCannotStopNewChatGeneration(t *testing.T) {
	r, commands := readySiteRobot()
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	if result := r.ExecuteCommand(context.Background(), Command{Action: "stop"}); result.State != "completed" {
		t.Fatal(result)
	}
	// Represent the new owner's reservation without reaching any motor API.
	r.mu.Lock()
	r.intentGeneration++
	r.intentActive = true
	count := len(commands.commands)
	r.mu.Unlock()
	owner.Stop(999999)
	if !r.intentActive || len(commands.commands) != count {
		t.Fatal("old cleanup changed a new chat generation")
	}
}

func TestLocalizationSearchOwnerHeartbeatCannotReviveExpiredMotion(t *testing.T) {
	r, commands := readySiteRobot()
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Look(1, 1); err != nil {
		t.Fatal(err)
	}
	r.heartbeat = time.Now().Add(-300 * time.Millisecond)
	old := r.heartbeat
	_ = r.Status()
	if r.heartbeat != old {
		t.Fatal("ordinary status started renewing lease")
	}
	if status := owner.Status(); !status.Moving || !r.heartbeat.After(old) {
		t.Fatal("live owner heartbeat did not renew its own lease")
	}
	r.heartbeat = time.Now().Add(-time.Second)
	status := owner.Status()
	if status.Moving || status.LookState != 3 || status.MotionRecoveryAllowed || !strings.Contains(status.Message, "map is no longer active") || len(commands.commands) != 1 {
		t.Fatalf("expired motion lease was revived: %+v", status)
	}
}

func TestLocalizationSearchOwnerKeepsFreshnessAndEpochGuards(t *testing.T) {
	for _, fault := range []string{"old pose", "old scan", "epoch", "stop unconfirmed", "already moving", "active chat"} {
		t.Run(fault, func(t *testing.T) {
			r, commands := readySiteRobot()
			if fault == "already moving" {
				r.moving = true
			}
			if fault == "active chat" {
				r.intentActive = true
			}
			owner, err := r.BeginLocalizationSearch(context.Background())
			if fault == "already moving" || fault == "active chat" {
				if err == nil || len(commands.commands) != 0 {
					t.Fatal("search took existing ownership")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			switch fault {
			case "old pose":
				r.observed.Pose.Received = time.Now().Add(-time.Second)
			case "old scan":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "epoch":
				r.poseEpoch++
			case "stop unconfirmed":
				r.stopUnconfirmed = true
			}
			if err := owner.Look(1, 1); err == nil || r.moving || len(commands.commands) != 0 {
				t.Fatal("search bypassed a controller guard")
			}
		})
	}
}

func TestLocalizationSearchOwnerMatchesAndConfirmsWithoutSequenceInflation(t *testing.T) {
	input, cloud := passiveLocalizationFixture(false)
	r, commands := readySiteRobot()
	r.commandSequence = 7000000
	r.frame = ""
	r.observed.Surfaces = cloud
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				r.mu.Lock()
				r.observed.Pose.Received, r.observed.Pose.Captured, r.observed.CloudAt = now, now, now
				r.mu.Unlock()
			}
		}
	}()
	defer func() { cancel(); <-done }()
	owner, err := r.BeginLocalizationSearch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	report, err := owner.FuseLocalization(123, input)
	if err != nil || len(report.Candidates) != 1 {
		t.Fatalf("owned matching failed: %v %+v", err, report)
	}
	if err := owner.ConfirmLocalization(124, 123, input.Frame, 0); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	status := r.Status()
	if !status.Aligned || status.Frame != input.Frame || r.commandSequence != 7000000 || r.intentActive || len(commands.commands) != 0 {
		t.Fatalf("owned confirmation altered control counter or issued commands: %+v", status)
	}
	r.Stop(7000001)
	if r.commandSequence != 7000001 {
		t.Fatal("normal next desktop command was poisoned")
	}
}

func TestLocalizationSearchOwnerDiscoveryRetainsBoundedHistory(t *testing.T) {
	r, _ := readySiteRobot()
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.DiscoveryStep(1, math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	if r.discoveryHistory.root != owner.generation || r.discoveryHistory.starts != 1 || r.commandSequence != 0 {
		t.Fatal("logical search root escaped into controller ownership")
	}
	r.mu.Lock()
	_ = r.stopLocked()
	r.discoveryHistory.starts = 3
	r.mu.Unlock()
	if err := owner.DiscoveryStep(2, math.MaxUint64); !errors.Is(err, ErrDiscoveryBlocked) {
		t.Fatal("search bypassed viewpoint movement cap", err)
	}
}
