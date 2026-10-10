package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

type hostOutcome int

const (
	hostStopped     hostOutcome = iota // the instance was asked to stop
	hostFailedFinal                    // reported failure or expired engine build
	hostCrashed                        // exited or went silent; worth a restart
)

const engineBuildTimeoutReason = "engine build took longer than 15 min"

// run takes an instance from preparation to removal, restarting a crashed or
// silent host up to maxRestarts times.
func (s *Supervisor) run(inst *instance) {
	final, detail := StateStopped, ""
	defer func() { s.finish(inst, final, detail) }()
	if err := s.prepare(inst); err != nil {
		if inst.ctx.Err() == nil {
			final, detail = StateFailed, err.Error()
		}
		return
	}
	for attempt := 0; ; attempt++ {
		outcome, reason := s.runHost(inst)
		switch outcome {
		case hostStopped:
			return
		case hostFailedFinal:
			final, detail = StateFailed, reason
			return
		}
		if attempt == maxRestarts {
			final, detail = StateFailed, fmt.Sprintf("host failed %d times; last: %s", attempt+1, reason)
			return
		}
		s.setState(inst, StateRestarting, reason)
		s.removeHost(inst)
		if !s.sleep(inst, restartBackoff[attempt]) {
			return
		}
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
	if s.needsEngineBuild(inst.variant) {
		select {
		case s.build <- struct{}{}:
		default:
			s.setState(inst, StatePreparing, "waiting for another engine build")
			select {
			case s.build <- struct{}{}:
			case <-inst.ctx.Done():
				return inst.ctx.Err()
			}
		}
		inst.mu.Lock()
		inst.holdsBuild = true
		inst.mu.Unlock()
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

// runHost starts one host and waits until the instance must stop, the host
// reports a failure, or it crashes or goes silent.
func (s *Supervisor) runHost(inst *instance) (hostOutcome, string) {
	// The camera's node can change or go while the instance lives, so every
	// start binds the node the camera has now. A camera that can no longer
	// stream is final, like a lost camera: a restart cannot bring it back.
	if err := s.reacquireCamera(inst); err != nil {
		if inst.ctx.Err() != nil {
			return hostStopped, ""
		}
		return hostFailedFinal, err.Error()
	}
	// Mark the host live before it starts, so an immediate "ready" counts.
	inst.mu.Lock()
	inst.hostRunning, inst.hostFailure, inst.building = true, "", time.Time{}
	inst.lastStatus = s.clock.Now()
	inst.setStateLocked(StateStarting, "")
	s.armStallLocked(inst)
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.hostRunning = false
		inst.mu.Unlock()
	}()

	exits, err := s.cfg.Runtime.StartHost(inst.ctx, s.hostSpec(inst))
	if err != nil {
		if inst.ctx.Err() != nil {
			return hostStopped, ""
		}
		return hostCrashed, "starting host: " + err.Error()
	}
	for {
		select {
		case <-inst.ctx.Done():
			return hostStopped, ""
		case exit := <-exits:
			if failure, duringBuild := inst.failure(); failure != "" {
				if duringBuild {
					failure += s.logTail(inst)
				}
				return hostFailedFinal, failure
			}
			return hostCrashed, fmt.Sprintf("host exited with status %d", exit.Code) + s.logTail(inst)
		case <-inst.wake:
			outcome, reason, done, attachLog := s.checkHost(inst)
			if !done {
				continue
			}
			if attachLog || reason == engineBuildTimeoutReason {
				reason += s.logTail(inst)
			}
			return outcome, reason
		}
	}
}

// reacquireCamera pins the instance's camera again and keeps the node it is
// on now for the next host. Acquire is idempotent per owner, and a failure
// leaves no pin behind.
func (s *Supervisor) reacquireCamera(inst *instance) error {
	ctx, cancel := context.WithTimeout(inst.ctx, cameraTimeout)
	defer cancel()
	node, err := s.cfg.Cameras.Acquire(ctx, inst.id, inst.camera)
	if err != nil {
		return cameraNotStreamable(err)
	}
	inst.mu.Lock()
	inst.node = node
	inst.mu.Unlock()
	return nil
}

// checkHost looks for a reported failure, an expired engine build, or a
// silent host. The fourth result reports whether a reported failure happened
// while this host's engine was still building, so runHost should attach the
// host's log tail (design §9) once it has released inst.mu.
func (s *Supervisor) checkHost(inst *instance) (hostOutcome, string, bool, bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	now := s.clock.Now()
	duringBuild := !inst.building.IsZero() && inst.state != StateReady
	switch {
	case inst.hostFailure != "":
		return hostFailedFinal, inst.hostFailure, true, duringBuild
	case duringBuild && now.Sub(inst.building) >= engineBuildTimeout:
		return hostFailedFinal, engineBuildTimeoutReason, true, false
	case now.Sub(inst.lastStatus) >= stallTimeout:
		return hostCrashed, "host sent no status for 15 s", true, false
	}
	return 0, "", false, false
}

// sleep waits d on the supervisor's clock; false means the instance was
// stopped first.
func (s *Supervisor) sleep(inst *instance, d time.Duration) bool {
	fired := make(chan struct{})
	t := s.clock.AfterFunc(d, func() { close(fired) })
	defer t.Stop()
	select {
	case <-fired:
		return true
	case <-inst.ctx.Done():
		return false
	}
}

// armStallLocked wakes the run loop once the host has been silent for
// stallTimeout; every status re-arms it. Caller holds inst.mu.
func (s *Supervisor) armStallLocked(inst *instance) {
	s.clock.AfterFunc(stallTimeout, inst.poke)
}

// releaseBuildLocked frees the engine build slot if this instance holds it.
// Caller holds inst.mu.
func (s *Supervisor) releaseBuildLocked(inst *instance) {
	if inst.holdsBuild {
		inst.holdsBuild = false
		<-s.build
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
	now := s.clock.Now()
	inst.lastStatus = now
	inst.stats = st.Stats
	s.armStallLocked(inst)
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
		if inst.building.IsZero() {
			inst.building = now
			s.clock.AfterFunc(engineBuildTimeout, inst.poke)
		}
		changed = inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		s.releaseBuildLocked(inst)
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
	s.releaseBuildLocked(inst)
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

// CleanupOrphans removes model hosts and run directories that a previous
// agent process left behind; leases do not survive a restart. Call it before
// serving.
func (s *Supervisor) CleanupOrphans(ctx context.Context) error {
	ids, err := s.cfg.Runtime.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("listing model hosts: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if err := s.cfg.Runtime.RemoveHost(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("removing model host %s: %w", id, err))
		}
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.Root, "run")); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
