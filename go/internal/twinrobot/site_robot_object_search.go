package twinrobot

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// ObjectSearchProgress describes observation and bounded exploration, never a
// model-generated destination. Visual detection alone cannot authorize approach.
type ObjectSearchProgress struct {
	Query      string     `json:"query"`
	ViewID     string     `json:"view_id,omitempty"`
	Views      int        `json:"views"`
	Turns      int        `json:"turns"`
	Viewpoints int        `json:"viewpoints"`
	Seen       bool       `json:"seen"`
	Navigable  bool       `json:"navigable"`
	Box        [4]float64 `json:"box,omitempty"`
}

// SearchCameraView is immutable evidence owned by this controller generation.
// It contains no room alignment and cannot authorize object approach.
type SearchCameraView struct {
	captured          time.Time
	pose              sitenav.Pose
	generation, epoch uint64
}

func (o *LocalizationSearchOwner) SearchCameraView(received int64, after time.Time) (*SearchCameraView, error) {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := o.checkLocked(true); err != nil {
		return nil, err
	}
	now := time.Now()
	if err := r.observed.Ready(now); err != nil {
		return nil, err
	}
	at := time.Unix(0, received).Add(-r.observed.ClockOffset)
	if !at.After(after) || now.Sub(at) > 3*time.Second || now.Sub(at) < -50*time.Millisecond {
		return nil, errors.New("search camera frame is not fresh after the requested stationary view")
	}
	pose := r.observed.Pose
	for _, p := range r.poseHistory {
		if math.Abs(p.Received.Sub(at).Seconds()) < math.Abs(pose.Received.Sub(at).Seconds()) {
			pose = p
		}
	}
	if math.Abs(pose.Received.Sub(at).Seconds()) > .15 || math.Abs(r.observed.CloudAt.Sub(at).Seconds()) > .75 {
		return nil, errors.New("search frame has no receipt-time-matched odometry and LiDAR")
	}
	view := &SearchCameraView{captured: at, pose: pose, generation: o.generation, epoch: o.epoch}
	if err := o.checkSearchCameraViewLocked(view); err != nil {
		return nil, err
	}
	return view, nil
}
func (o *LocalizationSearchOwner) checkSearchCameraViewLocked(view *SearchCameraView) error {
	if err := o.checkLocked(true); err != nil {
		return err
	}
	if view == nil || view.generation != o.generation || view.epoch != o.epoch {
		return errors.New("camera view belongs to another robot search")
	}
	r := o.robot
	if err := r.observed.Ready(time.Now()); err != nil {
		return err
	}
	p := r.observed.Pose
	if p.Position.Distance(view.pose.Position) > .03 || math.Abs(p.Height-view.pose.Height) > .03 || math.Abs(math.Remainder(p.Yaw-view.pose.Yaw, 2*math.Pi)) > .03 || math.Abs(p.Roll-view.pose.Roll) > .03 || math.Abs(p.Pitch-view.pose.Pitch) > .03 {
		return errors.New("Go2 moved after the search camera frame; its sighting is not current")
	}
	return nil
}
func (o *LocalizationSearchOwner) CheckSearchCameraView(view *SearchCameraView) error {
	o.robot.mu.Lock()
	defer o.robot.mu.Unlock()
	return o.checkSearchCameraViewLocked(view)
}

// These automatic exploration orders ignore manual allowances for unobserved
// floor. They retain the same bounded controller and owner/Stop precedence.
func (o *LocalizationSearchOwner) ObjectSearchLook(logical uint64, direction int) error {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := o.nextLocked(logical, true); err != nil {
		return err
	}
	r.heartbeat = time.Now()
	r.strictMeasuredFloor = true
	if err := r.lookLocked(direction); err != nil {
		r.strictMeasuredFloor = false
		return err
	}
	return nil
}
func (o *LocalizationSearchOwner) ObjectSearchStep(logical uint64) error {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := o.nextLocked(logical, true); err != nil {
		return err
	}
	r.heartbeat = time.Now()
	allowance := r.underBodyAllowance
	r.underBodyAllowance = false
	defer func() { r.underBodyAllowance = allowance }()
	return r.discoveryStepLocked(o.generation)
}

// FinishSearchCameraView linearizes a sighting against Stop/newer operators.
// Successful completion releases only this stationary owner's reservation.
func (o *LocalizationSearchOwner) FinishSearchCameraView(view *SearchCameraView) error {
	r := o.robot
	r.mu.Lock()
	if err := o.checkSearchCameraViewLocked(view); err != nil {
		r.mu.Unlock()
		return err
	}
	r.intentActive = false
	o.closed = true
	r.mu.Unlock()
	o.cancel()
	return nil
}

// CapturedAt is the validated frame receipt time in the controller clock.
func (v *SearchCameraView) CapturedAt() time.Time { return v.captured }
