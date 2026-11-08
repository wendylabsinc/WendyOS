package backend

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	"math"
	"time"
)

type SiteRobotCandidate struct{ X, Z, Yaw, FloorY, RMS, Coverage float64 }
type siteRobotLocalization struct {
	alignment             sitenav.Alignment
	pose                  sitenav.Pose
	frame                 string
	floorY, rms, coverage float64
	created               time.Time
	surfaceBased          bool
}

// A proposed alignment cannot move the robot. ConfirmLocalization is required
// before it can become the active navigation frame.
func (r *SiteRobot) Localize(sequence uint64, frame string, mesh []sitenav.Triangle) ([]SiteRobotCandidate, error) {
	r.mu.Lock()
	if sequence <= r.commandSequence || r.closed || r.ctx.Err() != nil || len(frame) == 0 || len(frame) > 200 {
		r.mu.Unlock()
		return nil, errors.New("This localization request is no longer active.")
	}
	r.commandSequence = sequence
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	r.localizations = nil
	r.preview = nil
	if err := r.observed.LocalizationReady(time.Now()); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	pose := r.observed.Pose
	points := append([]sitenav.Point(nil), r.observed.Landmarks...)
	clearFloor := r.observed.Grid.ObservedClearFloor()
	epoch := r.poseEpoch
	r.mu.Unlock()
	room, err := sitenav.RoomFromMesh(mesh)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	candidates, err := sitenav.MatchRoomWithFloor(ctx, room, points, pose.Position, clearFloor)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commandSequence != sequence || r.closed || epoch != r.poseEpoch || r.ctx.Err() != nil {
		return nil, errors.New("This localization request was superseded.")
	}
	if err := r.observed.LocalizationReady(time.Now()); err != nil {
		return nil, err
	}
	if !sameLocalizationPose(pose, r.observed.Pose) {
		return nil, errors.New("Go2 moved during matching. Keep it still and retry.")
	}
	r.localizationSequence = sequence
	var results []SiteRobotCandidate
	for _, c := range candidates {
		a := sitenav.Alignment{Origin: pose.Position, Site: c.Position, Rotation: c.Rotation}
		r.localizations = append(r.localizations, siteRobotLocalization{alignment: a, pose: pose, frame: frame, floorY: room.FloorY, rms: c.RMS, coverage: c.Coverage, created: time.Now()})
		p := a.ToSite(r.observed.Pose.Position)
		results = append(results, SiteRobotCandidate{p.X, -p.Y, r.observed.Pose.Yaw + c.Rotation, room.FloorY, c.RMS, c.Coverage})
	}
	return results, nil
}

func sameLocalizationPose(a, b sitenav.Pose) bool {
	return a.Position.Distance(b.Position) < .05 && math.Abs(math.Remainder(a.Yaw-b.Yaw, 2*math.Pi)) < .05
}

func (r *SiteRobot) ConfirmLocalization(sequence, matchSequence uint64, frame string, index int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence || r.commandSequence != matchSequence {
		return errors.New("This robot order was superseded.")
	}
	r.commandSequence = sequence
	if r.closed || r.ctx.Err() != nil || matchSequence != r.localizationSequence || index < 0 || index >= len(r.localizations) {
		return errors.New("This suggested position expired. Locate Go2 again.")
	}
	if r.moving || r.stopUnconfirmed {
		if err := r.stopLocked(); err != nil {
			return err
		}
	}
	c := r.localizations[index]
	if frame != c.frame || time.Since(c.created) > 30*time.Second || !sameLocalizationPose(c.pose, r.observed.Pose) {
		return errors.New("Go2 or the site changed. Locate Go2 again.")
	}
	ready := r.observed.LocalizationReady(time.Now())
	if c.surfaceBased {
		ready = r.observed.SurfaceReady(time.Now())
	}
	if err := ready; err != nil {
		return err
	}
	r.alignment = c.alignment
	r.frame = frame
	r.floorY = c.floorY
	r.preview = nil
	r.localizations = nil
	r.message = "LiDAR position confirmed."
	return nil
}
