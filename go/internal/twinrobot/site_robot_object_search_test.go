package twinrobot

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func TestObjectSearchCameraEvidenceNeedsNoRoomButBindsMotionAndTiming(t *testing.T) {
	for _, fault := range []string{"none", "old", "predemand", "mismatched", "height", "rotation", "moving", "superseded", "canceled", "epoch"} {
		t.Run(fault, func(t *testing.T) {
			r, _ := readySiteRobot()
			r.frame = ""
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner, err := r.BeginLocalizationSearch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			now := time.Now()
			r.observed.Pose.Received = now
			r.observed.Pose.Captured = now
			r.observed.CloudAt = now
			received, after := now.UnixNano(), now.Add(-time.Millisecond)
			switch fault {
			case "old":
				received = now.Add(-4 * time.Second).UnixNano()
				after = now.Add(-5 * time.Second)
			case "predemand":
				after = now.Add(time.Second)
			case "mismatched":
				received = now.Add(-time.Second).UnixNano()
				after = now.Add(-2 * time.Second)
			}
			view, err := owner.SearchCameraView(received, after)
			if fault == "old" || fault == "predemand" || fault == "mismatched" {
				if err == nil {
					t.Fatal("accepted invalid capture")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "height":
				r.observed.Pose.Height += .04
			case "rotation":
				r.observed.Pose.Yaw += .04
			case "moving":
				r.moving = true
			case "superseded":
				r.Stop(1)
			case "canceled":
				cancel()
			case "epoch":
				r.poseEpoch++
			}
			err = owner.FinishSearchCameraView(view)
			if fault == "none" {
				if err != nil || r.intentActive || r.frame != "" {
					t.Fatal("map-free evidence could not finish", err)
				}
			} else if err == nil {
				t.Fatal("invalidated evidence completed", fault)
			}
		})
	}
}
func TestObjectSearchIgnoresManualUnknownFloorAllowances(t *testing.T) {
	for _, action := range []string{"turn", "viewpoint"} {
		t.Run(action, func(t *testing.T) {
			r, commands := readySiteRobot()
			r.frame = ""
			r.allowUnobservedFloor = true
			r.underBodyAllowance = true
			r.observed.Grid.Floor = map[sitenav.Cell]bool{}
			owner, err := r.BeginLocalizationSearch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if action == "turn" {
				err = owner.ObjectSearchLook(1, 1)
			} else {
				err = owner.ObjectSearchStep(1)
			}
			if !errors.Is(err, ErrDiscoveryBlocked) || r.moving || len(commands.commands) != 0 || r.strictMeasuredFloor || !r.allowUnobservedFloor || !r.underBodyAllowance {
				t.Fatal("automatic search leaked manual allowance", err)
			}
			if action == "turn" && !strings.Contains(err.Error(), "unobserved floor") {
				t.Fatal("misreported unknown floor as obstacle", err)
			}
		})
	}
}
func TestObjectSearchTurnRetainsStrictFloorUntilStop(t *testing.T) {
	r, commands := readySiteRobot()
	r.frame = ""
	r.allowUnobservedFloor = true
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = owner.ObjectSearchLook(1, 1); err != nil {
		t.Fatal(err)
	}
	if !r.strictMeasuredFloor {
		t.Fatal("strict turn policy was not retained")
	}
	r.observed.Pose.Yaw = math.Pi / 12
	r.observed.Grid.Floor = map[sitenav.Cell]bool{}
	r.stepLocked(time.Now())
	if r.moving || r.lookState != 3 || r.strictMeasuredFloor || !strings.Contains(r.motionFailure, "unobserved floor") || len(commands.commands) != 1 {
		t.Fatal("automatic turn crossed unknown floor", r.motionFailure)
	}
}
func TestObjectSearchCompletionCannotReleaseNewOperator(t *testing.T) {
	r, commands := readySiteRobot()
	owner, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.observed.Pose.Received = now
	r.observed.Pose.Captured = now
	r.observed.CloudAt = now
	view, err := owner.SearchCameraView(now.UnixNano(), now.Add(-time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	r.Stop(1)
	owner.Close()
	newer, err := r.BeginLocalizationSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	before := len(commands.commands)
	if err = owner.FinishSearchCameraView(view); err == nil {
		t.Fatal(err)
	}
	owner.Close()
	if !r.intentActive || len(commands.commands) != before {
		t.Fatal("old sighting stopped or released newer owner")
	}
}
