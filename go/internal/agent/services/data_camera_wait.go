package services

import (
	"fmt"
	"time"
)

// ownCameraWait follows what a recording itself waited for the camera's
// previous capture to release the camera (holdCamera), across the hubs it
// reads in turn. A hub's own wait can have begun before the recording joined
// it, and is left unfinished when the hub is replaced before its turn.
type ownCameraWait struct {
	// since is when the recording began to depend on the camera's hub for its
	// frames. It stays when that hub is replaced: the recording is kept from
	// the camera from the moment the replacement began to wait.
	since time.Time
	// carried is when a wait began that a hub left behind never finished: the
	// capture that replaced it waits for the same camera. left is when the
	// recording left that hub.
	carried, left time.Time
	// hadCamera says a hub the recording has left had the camera, so that a
	// wait behind a later one is a gap in the recording and not a late start.
	hadCamera bool
}

type ownWaitState uint8

const (
	noOwnWait ownWaitState = iota
	ownWaitOver
	ownWaitUnfinished
)

// cameraWaitNoteFloor is the shortest wait a recording reports. Every capture
// that replaces another starts behind its return, and a wait shorter than a
// frame of the fastest cameras costs a recording nothing.
const cameraWaitNoteFloor = 10 * time.Millisecond

// of is the part of hub's wait for the camera that the recording sat through,
// as of now: from when, until when, and whether it is over. A wait under
// cameraWaitNoteFloor is none.
func (o ownCameraWait) of(hub *deviceHub, now time.Time) (from, until time.Time, state ownWaitState) {
	began, ended, _ := hub.cameraWait.read()
	from, until, state = o.carried, now, ownWaitUnfinished
	switch {
	case began.IsZero() && from.IsZero():
		return time.Time{}, time.Time{}, noOwnWait
	case began.IsZero():
		// The capture that replaced the one left waiting found the camera free.
		until, state = o.left, ownWaitOver
	case !ended.IsZero():
		until, state = ended, ownWaitOver
	}
	if from.IsZero() {
		from = o.since
		if began.After(from) {
			from = began
		}
	}
	if until.Sub(from) < cameraWaitNoteFloor {
		return time.Time{}, time.Time{}, noOwnWait
	}
	return from, until, state
}

// movedFrom is of(hub, now) as the recording leaves hub for the one that
// replaced it. gap says a wait that is over was a gap in the recording.
func (o *ownCameraWait) movedFrom(hub *deviceHub, now time.Time) (from, until time.Time, state ownWaitState, gap bool) {
	from, until, state = o.of(hub, now)
	gap = o.hadCamera
	o.carried, o.left = time.Time{}, time.Time{}
	if state == ownWaitUnfinished {
		o.carried, o.left = from, now
	}
	if began, ended, _ := hub.cameraWait.read(); began.IsZero() || !ended.IsZero() {
		o.hadCamera = true
	}
	return from, until, state, gap
}

const (
	cameraWaitedFor      = "this recording waited %s for the camera's previous capture to close the camera"
	cameraWaitAtStart    = cameraWaitedFor + "; recording started after it"
	cameraWaitInEpisode  = cameraWaitedFor + " after the capture it read from was replaced; no frames were recorded during that time"
	cameraWaitInPreRoll  = cameraWaitedFor + ", until %s before the trigger; its pre-roll holds no frame from before then"
	cameraStillHeld      = "the camera was still held by its previous capture"
	cameraStillHeldAtEnd = cameraStillHeld + " when the recording ended, %s after this recording began to wait for it; no frames were recorded during that time"
)

func roundedWait(from, until time.Time) time.Duration {
	return until.Sub(from).Round(time.Millisecond)
}

func waitOverNote(from, until time.Time, gap bool) string {
	if gap {
		return fmt.Sprintf(cameraWaitInEpisode, roundedWait(from, until))
	}
	return fmt.Sprintf(cameraWaitAtStart, roundedWait(from, until))
}

// noteCameraWaitOnLeaving puts a wait that is over on record as the recording
// leaves its hub for another. One left unfinished goes on behind the next.
func (c *cameraCapture) noteCameraWaitOnLeaving() {
	if from, until, state, gap := c.ownWait.movedFrom(c.hub, time.Now()); state == ownWaitOver {
		c.notes = append(c.notes, waitOverNote(from, until, gap))
	}
}

// noteCameraWaitAtEnd puts the recording's wait behind its last hub on record.
// Call it before leaving that hub, which ends a hub with no other subscriber
// and its wait with it.
func (c *cameraCapture) noteCameraWaitAtEnd() {
	switch from, until, state := c.ownWait.of(c.hub, time.Now()); state {
	case ownWaitOver:
		c.notes = append(c.notes, waitOverNote(from, until, c.ownWait.hadCamera))
	case ownWaitUnfinished:
		c.notes = append(c.notes, fmt.Sprintf(cameraStillHeldAtEnd, roundedWait(from, until)))
	}
}

// whyNoFirstFrame is what a recording that gave up waiting for its first
// frame after limit says of the camera's previous capture: that it still held
// the camera, or that the recording waited most of the limit for it. Call it
// once the recording's run has returned.
func (c *cameraCapture) whyNoFirstFrame(gaveUp time.Time, limit time.Duration) string {
	switch from, until, state := c.ownWait.of(c.hub, gaveUp); {
	case state == ownWaitUnfinished:
		return fmt.Sprintf(": %s, after this recording had waited %s for it", cameraStillHeld, roundedWait(from, until))
	case state == ownWaitOver && until.Sub(from) >= limit/2:
		return fmt.Sprintf(": "+cameraWaitedFor, fmt.Sprintf("%s of those %s", roundedWait(from, until), limit))
	}
	return ""
}
