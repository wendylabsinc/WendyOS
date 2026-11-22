package twinrobot

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

// ResetLocalization explicitly discards a rejected position. Even a stationary
// robot must acknowledge STOP before its navigation frame can be forgotten.
func (r *SiteRobot) ResetLocalization(sequence uint64, frame string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sequence <= r.commandSequence || r.closed || r.ctx.Err() != nil {
		return errors.New("This localization reset is no longer active.")
	}
	if frame == "" || len(frame) > 200 || r.frame != "" && r.frame != frame {
		return errors.New("The robot's room frame changed.")
	}
	r.commandSequence = sequence
	if err := r.stopLocked(); err != nil {
		return err
	}
	r.clearMotionFailureLocked()
	r.alignment = sitenav.Alignment{}
	r.frame = ""
	r.floorY = 0
	r.localizations = nil
	r.localizationSequence = 0
	r.fusionTracker = sitenav.PoseTracker{}
	r.fusionFrame = ""
	r.fusionEpoch = 0
	r.discoveryHistory = discoveryHistory{}
	r.discoveryState, r.lookState, r.lookDirection = 0, 0, 0
	r.goal, r.routeOrigin = sitenav.Point{}, sitenav.Point{}
	r.lastRouteRequest = nil
	r.message = "Position cleared. Find Go2 again."
	return nil
}
