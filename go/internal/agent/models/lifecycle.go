package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// run takes an instance from preparation to removal.
func (s *Supervisor) run(inst *instance) {
	final, detail := StateStopped, ""
	defer func() { s.finish(inst, final, detail) }()
	if err := s.prepare(inst); err != nil {
		if inst.ctx.Err() == nil {
			final, detail = StateFailed, err.Error()
		}
		return
	}
	if failed, reason := s.runHost(inst); failed {
		final, detail = StateFailed, reason
	}
}

// prepare pulls the host image, fetches the model file and writes the labels.
func (s *Supervisor) prepare(inst *instance) error {
	s.setState(inst, StatePreparing, "pulling host image")
	if err := s.cfg.Runtime.EnsureImage(inst.ctx, inst.variant.HostImage); err != nil {
		return fmt.Errorf("pulling host image: %w", err)
	}
	s.setState(inst, StatePreparing, "downloading model file")
	file, err := s.cfg.Files.Fetch(inst.ctx, inst.variant.File)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(inst.runDir, 0o755); err != nil {
		return err
	}
	labels := strings.Join(inst.model.Labels, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(inst.runDir, "labels.txt"), []byte(labels), 0o444); err != nil {
		return err
	}
	if inst.variant.Engine == EngineTensorRT {
		dir := filepath.Join(s.enginesDir(), inst.variant.File.SHA256)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The host runs unprivileged and writes the engine it builds here.
		if err := os.Chown(dir, HostUID, HostGID); err != nil && !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	inst.mu.Lock()
	inst.modelFile = file
	inst.mu.Unlock()
	return nil
}

func (s *Supervisor) setState(inst *instance, st State, detail string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.setStateLocked(st, detail)
}

// runHost starts the host and waits until the instance must stop or the host
// ends. It reports whether the instance failed, and why.
func (s *Supervisor) runHost(inst *instance) (bool, string) {
	// Mark the host live before it starts, so an immediate "ready" counts.
	inst.mu.Lock()
	inst.hostRunning, inst.hostFailure = true, ""
	inst.lastStatus = s.clock.Now()
	inst.setStateLocked(StateStarting, "")
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.hostRunning = false
		inst.mu.Unlock()
	}()

	exits, err := s.cfg.Runtime.StartHost(inst.ctx, s.hostSpec(inst))
	if err != nil {
		if inst.ctx.Err() != nil {
			return false, ""
		}
		return true, "starting host: " + err.Error()
	}
	for {
		select {
		case <-inst.ctx.Done():
			return false, ""
		case exit := <-exits:
			if failure := inst.failure(); failure != "" {
				return true, failure
			}
			return true, fmt.Sprintf("host exited with status %d", exit.Code) + s.logTail(inst)
		case <-inst.wake:
			if failure := inst.failure(); failure != "" {
				return true, failure
			}
		}
	}
}

func (s *Supervisor) hostSpec(inst *instance) HostSpec {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	spec := HostSpec{
		InstanceID: inst.id, AppID: AppIDPrefix + inst.id, Image: inst.variant.HostImage,
		Engine: inst.variant.Engine, ModelID: inst.model.ID, VariantID: inst.variant.ID,
		FileSHA256: inst.variant.File.SHA256, ModelFile: inst.modelFile,
		LabelsFile: filepath.Join(inst.runDir, "labels.txt"), CameraNode: inst.node,
		CameraSource: inst.camera, LogPath: filepath.Join(inst.runDir, "host.log"),
	}
	if inst.variant.Engine == EngineTensorRT {
		spec.EngineCache = filepath.Join(s.enginesDir(), inst.variant.File.SHA256)
	}
	return spec
}

// handleStatus applies a model.status record from the live host.
func (s *Supervisor) handleStatus(inst *instance, st HostStatus) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.hostRunning {
		return // a host being replaced or removed no longer speaks for the instance
	}
	inst.lastStatus = s.clock.Now()
	inst.stats = st.Stats
	changed := false
	switch st.State {
	case HostFailed:
		inst.hostFailure = st.Reason
		if inst.hostFailure == "" {
			inst.hostFailure = "the model host reported a failure"
		}
		inst.poke()
		return
	case HostBuildingEngine:
		changed = inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		changed = inst.setStateLocked(StateReady, "")
	}
	if !changed {
		inst.broadcastLocked() // a heartbeat carries fresh stats
	}
}

// logTail returns the end of the host's output, for failure details.
func (s *Supervisor) logTail(inst *instance) string {
	f, err := os.Open(filepath.Join(inst.runDir, "host.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	const limit = 4096
	if info, err := f.Stat(); err == nil && info.Size() > limit {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return ""
		}
	}
	tail, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil || len(tail) == 0 {
		return ""
	}
	return "\n" + strings.TrimSpace(string(tail))
}

// finish removes the host and everything the instance held, then ends every
// watch with the final state.
func (s *Supervisor) finish(inst *instance, final State, detail string) {
	// A finishing instance, including a failed one, must not be reused or
	// watched while its host is removed.
	inst.cancel()
	s.removeHost(inst)
	s.cfg.Cameras.Release(context.Background(), inst.id)
	if err := os.RemoveAll(inst.runDir); err != nil {
		s.log.Warn("removing a model run directory failed", zap.String("instance", inst.id), zap.Error(err))
	}
	s.forget(inst)
	inst.mu.Lock()
	s.cancelGraceLocked(inst)
	if final == StateStopped && detail == "" {
		detail = inst.stopReason
	}
	// Set directly instead of broadcasting: each watch gets the final state
	// exactly once, through Final after its channel closes.
	inst.state, inst.detail = final, detail
	end := inst.infoLocked()
	for id, w := range inst.watches {
		snapshot := end
		w.end(&snapshot)
		delete(inst.watches, id)
	}
	inst.mu.Unlock()
	close(inst.done)
	s.log.Info("model instance ended", zap.String("instance", inst.id), zap.Stringer("state", final), zap.String("detail", detail))
}

func (s *Supervisor) removeHost(inst *instance) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := s.cfg.Runtime.RemoveHost(ctx, inst.id); err != nil {
		s.log.Warn("removing a model host failed", zap.String("instance", inst.id), zap.Error(err))
	}
}
