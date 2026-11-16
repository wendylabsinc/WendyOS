package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// The chip holds one sketch, so it belongs to one app at a time: the app whose
// deploy flashed it last.

const (
	mcuOwnerFeaturePrefix = "mcu-sketch-owner:"
	// Set next to the owner while an interrupted flash may have damaged its sketch.
	mcuContentUnknownFeature = "mcu-sketch-unknown"
)

// sketchMarker is an empty file per app that has a sketch.
func (s *McuService) sketchMarker(appID string) string {
	return filepath.Join(filepath.Dir(s.statePath), "apps", appID)
}

// AnySketchApp is false on devices where no app has a sketch.
func (s *McuService) AnySketchApp() bool {
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(s.statePath), "apps"))
	return err == nil && len(entries) > 0
}

// ownerIs reports whether the chip is known to hold appID's sketch.
func (s *McuService) ownerIs(appID string) bool {
	st, owned := s.readState()
	return owned && st.AppID == appID && !st.ContentUnknown
}

func (s *McuService) readState() (mcuState, bool) {
	var st mcuState
	data, err := os.ReadFile(s.statePath)
	if err != nil || json.Unmarshal(data, &st) != nil || st.AppID == "" {
		return mcuState{}, false
	}
	return st, true
}

// mcuOwnerFeature tells the CLI which app's sketch is on the chip: deploying
// another sketch stops that app, so its ports are not a conflict.
func mcuOwnerFeature() []string {
	var st mcuState
	data, err := os.ReadFile(mcuStatePath)
	if err != nil || json.Unmarshal(data, &st) != nil || st.AppID == "" {
		return nil
	}
	features := []string{mcuOwnerFeaturePrefix + st.AppID}
	if st.ContentUnknown {
		features = append(features, mcuContentUnknownFeature)
	}
	return features
}

// markSketchApp notes that appID has a sketch, so its start is checked.
func (s *McuService) markSketchApp(appID string) {
	path := s.sketchMarker(appID)
	if _, err := os.Stat(path); err == nil {
		return
	}
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}
	if err != nil {
		s.logger.Warn("Failed to note the app's microcontroller sketch", zap.String("app_id", appID), zap.Error(err))
	}
}

func (s *McuService) hasSketch(appID string) bool {
	if appconfig.ValidateAppID(appID) != nil {
		return false
	}
	_, err := os.Stat(s.sketchMarker(appID))
	return err == nil
}

// SketchStartRefusal says why appID must not start now, or "". An app with a
// sketch only works while its own sketch is on the chip, and only a deploy
// puts it there: a start never flashes.
func (s *McuService) SketchStartRefusal(appID string) string {
	if !s.hasSketch(appID) || s.ownerIs(appID) {
		return ""
	}
	again := fmt.Sprintf("deploy %s again with wendy run to put its sketch back", appID)
	st, owned := s.readState()
	switch {
	case owned && st.AppID != appID:
		return fmt.Sprintf("the board's microcontroller holds the sketch of %s, not of this app; %s", st.AppID, again)
	case owned:
		return fmt.Sprintf("an interrupted flash may have damaged this app's sketch on the board's microcontroller; %s", again)
	}
	return fmt.Sprintf("the board's microcontroller holds no sketch; %s", again)
}

// stopReplaced stops previous, the app whose sketch a flash for owner just
// replaced. Call it without the flash lock: a stop can take long.
func (s *McuService) stopReplaced(ctx context.Context, previous, owner string, unrecorded bool) {
	if previous == "" || previous == owner || s.stopApp == nil {
		return
	}
	// Another flash may have given the chip back since.
	if now, owned := s.readState(); !unrecorded && owned && now.AppID == previous && !now.ContentUnknown {
		return
	}
	if err := s.stopApp(ctx, previous); err != nil {
		s.logger.Warn("Failed to stop the app whose sketch was replaced", zap.String("app_id", previous), zap.Error(err))
	}
}

// ForgetApp forgets a removed app's sketch. If it is on the chip it is erased
// too: a removed app must not leave code running.
func (s *McuService) ForgetApp(ctx context.Context, appID string) {
	if appconfig.ValidateAppID(appID) != nil {
		return
	}
	owner, owned := s.readState()
	if !s.hasSketch(appID) && (!owned || owner.AppID != appID) {
		return
	}
	if !s.lock(ctx, nil) {
		s.logger.Warn("Microcontroller busy; the removed app's sketch stays until the next flash", zap.String("app_id", appID))
		return
	}
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.sketchMarker(appID)); err != nil {
		s.logger.Warn("Failed to forget the app's microcontroller sketch", zap.String("app_id", appID), zap.Error(err))
	}
	// Another app may have flashed its sketch while this waited.
	if owner, owned = s.readState(); !owned || owner.AppID != appID {
		return
	}
	defer s.holdUpdater(s.logger)()
	eraseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.helperTimeout)
	defer cancel()
	out, wait, err := s.runHelper(eraseCtx, "--erase", owner.Layout)
	if err == nil {
		_, _ = io.Copy(io.Discard, out)
		_ = out.Close()
		err = wait()
	}
	if err != nil {
		// The sketch is still on the chip, so the record stays true.
		s.logger.Warn("Failed to erase the removed app's sketch", zap.String("app_id", appID), zap.Error(err))
		return
	}
	if err := os.Remove(s.statePath); err != nil {
		s.logger.Warn("Failed to clear the microcontroller owner", zap.Error(err))
	}
}
