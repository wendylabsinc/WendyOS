package twinrobot

import (
	"context"
	"errors"
	"time"
)

var ErrLocalizationSearchSuperseded = errors.New("Localization search was superseded by another robot command.")

// LocalizationSearchOwner adapts the bounded desktop search algorithm to a
// headless chat goal. Its logical counters never advance commandSequence, so
// the next real desktop order can still interrupt it. All fields other than
// the immutable robot/context/cancel references are protected by robot.mu.
type LocalizationSearchOwner struct {
	robot                             *SiteRobot
	ctx                               context.Context
	cancel                            context.CancelFunc
	generation, sequence, epoch       uint64
	logical, matchLogical, matchToken uint64
	closed                            bool
}

func (r *SiteRobot) BeginLocalizationSearch(ctx context.Context) (*LocalizationSearchOwner, error) {
	life, cancel := context.WithTimeout(ctx, 5*time.Minute)
	r.mu.Lock()
	if err := r.passiveLocalizationReadyLocked(life); err != nil {
		r.mu.Unlock()
		cancel()
		return nil, err
	}
	r.intentGeneration++
	r.intentActive = true
	r.heartbeat = time.Now()
	r.localizations, r.preview = nil, nil
	r.discoveryHistory = discoveryHistory{}
	owner := &LocalizationSearchOwner{robot: r, ctx: life, cancel: cancel, generation: r.intentGeneration, sequence: r.commandSequence, epoch: r.poseEpoch}
	r.mu.Unlock()
	context.AfterFunc(life, owner.Close)
	return owner, nil
}

func (o *LocalizationSearchOwner) ownsLocked() bool {
	r := o.robot
	return !o.closed && r.intentActive && r.intentGeneration == o.generation && r.commandSequence == o.sequence
}

func (o *LocalizationSearchOwner) checkLocked(stationary bool) error {
	if err := o.ctx.Err(); err != nil {
		return err
	}
	r := o.robot
	if !o.ownsLocked() || r.poseEpoch != o.epoch {
		return ErrLocalizationSearchSuperseded
	}
	if r.closed || r.ctx.Err() != nil {
		return errors.New("Go2 observations are disconnected.")
	}
	if r.stopUnconfirmed {
		return errors.New(stopUnconfirmedMessage)
	}
	if stationary && (r.moving || r.lookTurn != nil || r.discoveryMove != nil) {
		return errors.New("Wait for the current search movement to finish before matching or starting another step.")
	}
	return nil
}

func (o *LocalizationSearchOwner) nextLocked(logical uint64, stationary bool) error {
	if err := o.checkLocked(stationary); err != nil {
		return err
	}
	if logical <= o.logical {
		return ErrLocalizationSearchSuperseded
	}
	o.logical = logical
	return nil
}

func (o *LocalizationSearchOwner) Check() error {
	o.robot.mu.Lock()
	defer o.robot.mu.Unlock()
	return o.checkLocked(false)
}

// Status is the search's control heartbeat, unlike SiteRobot.Status, which
// remains strictly read-only. A lost owner cannot renew another operator's
// lease. An already-expired movement lease cannot be revived by polling.
func (o *LocalizationSearchOwner) Status() SiteRobotSnapshot {
	r := o.robot
	r.mu.Lock()
	err := o.checkLocked(false)
	if err == nil {
		now := time.Now()
		if now.Sub(r.heartbeat) > 600*time.Millisecond {
			r.motionRecoveryAllowed = false
			if r.moving {
				r.message = MovementLeaseLostMessage
				_ = r.stopLocked()
			}
		}
		r.heartbeat = now
	}
	r.mu.Unlock()
	s := r.Status()
	if err != nil {
		// This snapshot belongs only to the private search adapter. Returning
		// a terminal flag stops that search; it does not close the live robot.
		s.Disconnected = true
		s.Message = err.Error()
	}
	return s
}

func (o *LocalizationSearchOwner) FuseLocalization(logical uint64, input SiteFusionRequest) (SiteFusionReport, error) {
	return o.robot.fuseLocalizationOwned(logical, input, nil, o)
}

func (o *LocalizationSearchOwner) Look(logical uint64, direction int) error {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := o.nextLocked(logical, true); err != nil {
		return err
	}
	r.heartbeat = time.Now()
	return r.lookLocked(direction)
}

func (o *LocalizationSearchOwner) DiscoveryStep(logical, _ uint64) error {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := o.nextLocked(logical, true); err != nil {
		return err
	}
	r.heartbeat = time.Now()
	return r.discoveryStepLocked(o.generation)
}

func (o *LocalizationSearchOwner) ConfirmLocalization(logical, matchLogical uint64, frame string, index int) error {
	r := o.robot
	r.mu.Lock()
	defer r.mu.Unlock()
	if matchLogical != o.matchLogical || o.logical != matchLogical || o.matchToken == 0 {
		return ErrLocalizationSearchSuperseded
	}
	if err := o.nextLocked(logical, true); err != nil {
		return err
	}
	return r.confirmLocalizationCandidateLocked(o.matchToken, frame, index, false)
}

// Stop's logical value may contain the desktop search's large cleanup stride.
// It is never forwarded to the robot counter or allowed to stop a newer owner.
func (o *LocalizationSearchOwner) Stop(_ uint64) { o.Close() }

func (o *LocalizationSearchOwner) Close() {
	r := o.robot
	r.mu.Lock()
	if o.closed {
		r.mu.Unlock()
		return
	}
	if o.ownsLocked() {
		r.motionRecoveryAllowed = false
		if r.moving || r.stopUnconfirmed {
			_ = r.stopLocked()
		}
	}
	// A desktop order changes commandSequence without changing the chat
	// generation. Release this old chat reservation without stopping it.
	if r.intentGeneration == o.generation {
		r.intentActive = false
	}
	o.closed = true
	r.mu.Unlock()
	o.cancel()
}
