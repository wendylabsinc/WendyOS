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
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// The chip holds one sketch, so it belongs to one app at a time. Each app's
// last firmware is kept, so starting an app can put its sketch back.

const mcuOwnerFeaturePrefix = "mcu-sketch-owner:"

func (s *McuService) firmwareDir(appID string) string {
	return filepath.Join(filepath.Dir(s.statePath), "apps", appID)
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
	return []string{mcuOwnerFeaturePrefix + st.AppID}
}

// keepFirmware stores the images just flashed as the app's own.
func (s *McuService) keepFirmware(spec *agentpbv2.McuFirmwareSpec, loaderPath, sketchPath string) {
	dir := s.firmwareDir(spec.GetAppId())
	tmp := dir + ".tmp"
	err := func() error {
		if err := os.RemoveAll(tmp); err != nil {
			return err
		}
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			return err
		}
		for name, src := range map[string]string{"loader.elf": loaderPath, "sketch.bin": sketchPath} {
			if err := copyFile(src, filepath.Join(tmp, name)); err != nil {
				return err
			}
		}
		meta := mcuState{AppID: spec.GetAppId(), Layout: spec.GetLayout(),
			LoaderSHA256: spec.GetLoader().GetSha256(), SketchSHA256: spec.GetSketch().GetSha256()}
		if err := writeMCUState(filepath.Join(tmp, "meta.json"), meta); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		return os.Rename(tmp, dir)
	}()
	if err != nil {
		s.logger.Warn("Failed to keep the app's microcontroller firmware", zap.String("app_id", spec.GetAppId()), zap.Error(err))
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (s *McuService) stopReplaced(ctx context.Context, previous, owner string) {
	if previous == "" || previous == owner || s.stopApp == nil {
		return
	}
	if err := s.stopApp(ctx, previous); err != nil {
		s.logger.Warn("Failed to stop the app whose sketch was replaced", zap.String("app_id", previous), zap.Error(err))
	}
}

// RestoreSketch puts an app's own sketch back on the chip before the app
// starts, when another app's sketch is there. Apps without a sketch, and the
// current owner, return at once.
func (s *McuService) RestoreSketch(ctx context.Context, appID string) error {
	if appconfig.ValidateAppID(appID) != nil {
		return nil
	}
	dir := s.firmwareDir(appID)
	var meta mcuState
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(data, &meta) != nil {
		return nil
	}
	owner, owned := s.readState()
	if owned && owner.AppID == appID && owner.SketchSHA256 == meta.SketchSHA256 && owner.LoaderSHA256 == meta.LoaderSHA256 {
		return nil
	}
	if _, ok := findMCULayout(s.layouts(), meta.Layout); !ok {
		return fmt.Errorf("this device can no longer flash the sketch of %s (layout %q); deploy it again with wendy run", appID, meta.Layout)
	}
	if !s.lock(ctx, nil) {
		return fmt.Errorf("the microcontroller is busy with another flash; start %s again when it finishes", appID)
	}
	defer s.mu.Unlock()
	defer s.holdUpdater(s.logger)()

	flashCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.helperTimeout)
	defer cancel()
	if _, err := s.flash(flashCtx, nil, meta.Layout, filepath.Join(dir, "loader.elf"), filepath.Join(dir, "sketch.bin")); err != nil {
		return fmt.Errorf("putting the sketch of %s back on the microcontroller: %w", appID, err)
	}
	meta.FlashedAt = ""
	previous := s.recordOwner(meta)
	s.logger.Info("Restored the app's microcontroller sketch", zap.String("app_id", appID), zap.String("replaced", previous))
	s.stopReplaced(flashCtx, previous, appID)
	return nil
}

// ForgetApp drops a removed app's firmware. If its sketch is on the chip, the
// sketch is erased too: a removed app must not leave code running.
func (s *McuService) ForgetApp(ctx context.Context, appID string) {
	if appconfig.ValidateAppID(appID) != nil {
		return
	}
	if err := os.RemoveAll(s.firmwareDir(appID)); err != nil {
		s.logger.Warn("Failed to remove the app's microcontroller firmware", zap.String("app_id", appID), zap.Error(err))
	}
	owner, owned := s.readState()
	if !owned || owner.AppID != appID {
		return
	}
	if !s.lock(ctx, nil) {
		s.logger.Warn("Microcontroller busy; the removed app's sketch stays until the next flash", zap.String("app_id", appID))
		return
	}
	defer s.mu.Unlock()
	// Another app may have flashed its sketch while this waited.
	if owner, owned = s.readState(); !owned || owner.AppID != appID {
		return
	}
	eraseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.helperTimeout)
	defer cancel()
	out, wait, err := s.runHelper(eraseCtx, "--erase", owner.Layout)
	if err == nil {
		_, _ = io.Copy(io.Discard, out)
		_ = out.Close()
		err = wait()
	}
	if err != nil {
		s.logger.Warn("Failed to erase the removed app's sketch", zap.String("app_id", appID), zap.Error(err))
		return
	}
	if err := os.Remove(s.statePath); err != nil {
		s.logger.Warn("Failed to clear the microcontroller owner", zap.Error(err))
	}
}
