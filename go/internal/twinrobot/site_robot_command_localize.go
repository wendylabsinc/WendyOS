package twinrobot

import (
	"context"
	"errors"
)

type passiveLocalizationRequest struct {
	ctx                         context.Context
	sequence, generation, epoch uint64
}

func (r *SiteRobot) passiveLocalizationReadyLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed || r.ctx.Err() != nil || r.streamDisconnected {
		return errors.New("Go2 observations are disconnected.")
	}
	if r.intentActive || r.moving || r.lookTurn != nil || r.discoveryMove != nil || r.stopUnconfirmed {
		return errors.New("Passive localization requires a stationary Go2 with no pending movement and an acknowledged stop.")
	}
	return nil
}

// LocalizeCommand matches current measurements against an existing room. It
// never searches by moving, sends a motion/Stop command, or accepts a guessed
// map pose. The shared tracker must select one reliable candidate before the
// existing freshness/epoch/pose confirmation checks can establish alignment.
func (r *SiteRobot) LocalizeCommand(ctx context.Context, input SiteFusionRequest) CommandResult {
	result := func(state, message string) CommandResult {
		return CommandResult{State: state, Message: message, Snapshot: r.Status()}
	}
	if err := ctx.Err(); err != nil {
		return result("canceled", err.Error())
	}
	if err := ValidateSearchGeometry(input); err != nil {
		return result("rejected", err.Error())
	}
	input.Explore = false
	input.Visual = nil // No old client camera hint is reused by a chat request.
	passive := &passiveLocalizationRequest{ctx: ctx}
	report, err := r.fuseLocalization(0, input, passive)
	if err != nil {
		state := "rejected"
		if ctx.Err() != nil {
			state = "canceled"
		}
		return result(state, err.Error())
	}
	selected := report.Tracking.Selected
	if report.WaitingForObservations || len(report.Candidates) != 1 || selected < 0 || selected >= len(report.Tracking.Fits) || !report.Tracking.Fits[selected].Valid {
		message := report.Message
		if message == "" {
			message = "Current observations do not identify one reliable room position."
		}
		return result("rejected", message+" No alignment was confirmed and no movement was requested.")
	}
	r.mu.Lock()
	err = r.confirmPassiveLocalizationLocked(passive, input.Frame)
	r.mu.Unlock()
	if err != nil {
		state := "rejected"
		if ctx.Err() != nil {
			state = "canceled"
		}
		return result(state, err.Error())
	}
	return result("completed", "Go2's position in the digital twin was confirmed from current LiDAR observations. No movement was requested.")
}

func (r *SiteRobot) confirmPassiveLocalizationLocked(passive *passiveLocalizationRequest, frame string) error {
	if err := r.passiveLocalizationReadyLocked(passive.ctx); err != nil {
		return err
	}
	if r.intentGeneration != passive.generation || r.commandSequence != passive.sequence || r.poseEpoch != passive.epoch {
		return errors.New("Passive localization was superseded by another robot command.")
	}
	return r.confirmLocalizationLocked(passive.sequence+1, passive.sequence, frame, 0, true)
}
