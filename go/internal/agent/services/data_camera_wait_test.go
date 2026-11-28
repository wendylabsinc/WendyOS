package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// hubThatWaited is a hub whose capture began to wait for the camera at began
// and had it at ended; a zero time is a step that never came.
func hubThatWaited(began, ended time.Time) *deviceHub {
	h := &deviceHub{}
	h.cameraWait.began, h.cameraWait.ended = began, ended
	return h
}

// A recording's own wait is the part of its hub's wait it sat through, and
// what it carried from a hub replaced before its turn came.
func TestOwnCameraWait_IsThePartOfTheHubsWaitTheRecordingSatThrough(t *testing.T) {
	at := func(d time.Duration) time.Time { return time.Unix(1_700_000_000, 0).Add(d) }
	none := time.Time{}
	now := at(time.Hour)
	for name, tc := range map[string]struct {
		own         ownCameraWait
		began       time.Time
		ended       time.Time
		from, until time.Time
		state       ownWaitState
	}{
		"a hub that never waited": {ownCameraWait{since: at(0)}, none, none, none, none, noOwnWait},
		"a wait the recording saw from its start": {
			ownCameraWait{since: at(0)}, at(time.Second), at(3 * time.Second), at(time.Second), at(3 * time.Second), ownWaitOver},
		"a wait already going on when the recording joined": {
			ownCameraWait{since: at(5 * time.Second)}, at(0), at(6 * time.Second), at(5 * time.Second), at(6 * time.Second), ownWaitOver},
		"a wait that was over when the recording joined": {
			ownCameraWait{since: at(5 * time.Second)}, at(0), at(2 * time.Second), none, none, noOwnWait},
		"a wait that is not over": {
			ownCameraWait{since: at(0)}, at(time.Second), none, at(time.Second), now, ownWaitUnfinished},
		"a wait carried from a hub replaced before its turn": {
			ownCameraWait{since: at(0), carried: at(time.Second), left: at(2 * time.Second)}, at(2 * time.Second), at(4 * time.Second),
			at(time.Second), at(4 * time.Second), ownWaitOver},
		"a carried wait behind a hub that found the camera free": {
			ownCameraWait{since: at(0), carried: at(time.Second), left: at(2 * time.Second)}, none, none,
			at(time.Second), at(2 * time.Second), ownWaitOver},
		"a wait of the floor is reported": {
			ownCameraWait{since: at(0)}, at(time.Second), at(time.Second + 10*time.Millisecond),
			at(time.Second), at(time.Second + 10*time.Millisecond), ownWaitOver},
		"a wait under the floor is not, so none prints as 0s": {
			ownCameraWait{since: at(0)}, at(time.Second), at(time.Second + 400*time.Microsecond), none, none, noOwnWait},
		"nor is one just under it": {
			ownCameraWait{since: at(0)}, at(time.Second), at(time.Second + 10*time.Millisecond - time.Nanosecond), none, none, noOwnWait},
	} {
		t.Run(name, func(t *testing.T) {
			from, until, state := tc.own.of(hubThatWaited(tc.began, tc.ended), now)
			if !from.Equal(tc.from) || !until.Equal(tc.until) || state != tc.state {
				t.Fatalf("own wait = %v to %v, state %d; want %v to %v, state %d", from, until, state, tc.from, tc.until, tc.state)
			}
		})
	}
}

// Leaving a hub carries a wait it never finished to the hub that replaced it,
// and marks every wait after a hub that had the camera as a gap.
func TestOwnCameraWait_LeavingAHubCarriesAnUnfinishedWaitAndMarksLaterOnesAsGaps(t *testing.T) {
	at := func(d time.Duration) time.Time { return time.Unix(1_700_000_000, 0).Add(d) }
	own := ownCameraWait{since: at(0)}

	_, _, state, gap := own.movedFrom(hubThatWaited(at(time.Second), time.Time{}), at(3*time.Second))
	if state != ownWaitUnfinished || gap || !own.carried.Equal(at(time.Second)) || !own.left.Equal(at(3*time.Second)) || own.hadCamera {
		t.Fatalf("leaving a hub that never had the camera: state %d, gap %v, then %+v; want its wait carried and no camera had", state, gap, own)
	}
	from, until, state, gap := own.movedFrom(hubThatWaited(at(3*time.Second), at(4*time.Second)), at(9*time.Second))
	if state != ownWaitOver || gap || !from.Equal(at(time.Second)) || !until.Equal(at(4*time.Second)) || !own.carried.IsZero() || !own.hadCamera {
		t.Fatalf("leaving the hub that finished the carried wait: %v to %v, state %d, gap %v, then %+v; want the whole wait, a late start", from, until, state, gap, own)
	}
	if _, _, state, gap = own.movedFrom(hubThatWaited(at(9*time.Second), at(10*time.Second)), at(20*time.Second)); state != ownWaitOver || !gap {
		t.Fatalf("a wait after a hub that had the camera: state %d, gap %v; want it over and a gap", state, gap)
	}
}

// One look at a wait is consistent: it is going on, or it is over with the
// camera, or it was given up, never none of these.
func TestCameraWait_ReadsEachStepWhole(t *testing.T) {
	var w cameraWait
	if began, ended, waiting := w.read(); !began.IsZero() || !ended.IsZero() || waiting {
		t.Fatalf("a capture that has not waited reads began=%v ended=%v waiting=%v", began, ended, waiting)
	}
	w.begin()
	if began, ended, waiting := w.read(); began.IsZero() || !ended.IsZero() || !waiting {
		t.Fatalf("a capture that waits reads began=%v ended=%v waiting=%v", began, ended, waiting)
	}
	w.cameraTaken()
	if _, ended, waiting := w.read(); ended.IsZero() || waiting {
		t.Fatalf("a capture that has the camera reads ended=%v waiting=%v", ended, waiting)
	}
	var gaveUp cameraWait
	gaveUp.begin()
	gaveUp.giveUp()
	if _, ended, waiting := gaveUp.read(); !ended.IsZero() || waiting {
		t.Fatalf("a capture whose hub ended first reads ended=%v waiting=%v", ended, waiting)
	}
}

// endsUnseen is a context that ends without its Done channel saying so, as one
// does for a select that has already chosen another case.
type endsUnseen struct {
	context.Context
	ended atomic.Bool
}

func (c *endsUnseen) Done() <-chan struct{} { return nil }

func (c *endsUnseen) Err() error {
	if c.ended.Load() {
		return context.Canceled
	}
	return nil
}

// A hub can end in the same moment its capture's turn comes. Its capture then
// did not get the camera, and its wait must not read as one that did.
func TestHoldCamera_ATurnThatComesAsTheHubEndsIsNotRecordedAsTheCameraTaken(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	t.Cleanup(svc.Shutdown)
	var first, second cameraWait
	release, held := svc.holdCamera(context.Background(), exclusiveCameraPath, &first)
	if !held {
		t.Fatal("the first capture was not given the camera")
	}
	hub := &endsUnseen{Context: context.Background()}
	turn := make(chan bool, 1)
	go func() {
		_, held := svc.holdCamera(hub, exclusiveCameraPath, &second)
		turn <- held
	}()
	waitUntil(t, "the second capture waits for the camera", func() bool {
		_, _, waiting := second.read()
		return waiting
	})
	hub.ended.Store(true)
	release()
	if <-turn {
		t.Fatal("a capture whose hub had ended was given the camera")
	}
	if began, ended, waiting := second.read(); began.IsZero() || waiting || !ended.IsZero() {
		t.Fatalf("the wait of a capture whose hub ended as its turn came reads began=%v ended=%v waiting=%v; want one that began, is not going on and never had the camera",
			began, ended, waiting)
	}
	next, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if release, held := svc.holdCamera(next, exclusiveCameraPath, &cameraWait{}); !held {
		t.Fatal("the turn the ended hub gave back was not there for the next capture")
	} else {
		release()
	}
}

// A hub that ends while its capture waits no longer reads as waiting.
func TestHoldCamera_AHubThatEndsWhileItsCaptureWaitsNoLongerReadsAsWaiting(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	t.Cleanup(svc.Shutdown)
	release, held := svc.holdCamera(context.Background(), exclusiveCameraPath, &cameraWait{})
	if !held {
		t.Fatal("the first capture was not given the camera")
	}
	defer release()
	var wait cameraWait
	hub, end := context.WithCancel(context.Background())
	turn := make(chan bool, 1)
	go func() {
		_, held := svc.holdCamera(hub, exclusiveCameraPath, &wait)
		turn <- held
	}()
	waitUntil(t, "the second capture waits for the camera", func() bool {
		_, _, waiting := wait.read()
		return waiting
	})
	end()
	if <-turn {
		t.Fatal("a capture whose hub had ended was given the camera")
	}
	if began, ended, waiting := wait.read(); began.IsZero() || waiting || !ended.IsZero() {
		t.Fatalf("the wait reads began=%v ended=%v waiting=%v; want one that began, is not going on and never had the camera", began, ended, waiting)
	}
}
