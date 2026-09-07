package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// vmCommand builds the emulator command; a seam so tests can launch something
// cheaper than QEMU. Deliberately exec.Command and not exec.CommandContext: a
// context-bound child dies with the CLI, which is exactly what detaching must
// prevent.
var vmCommand = exec.Command

// vmCommandContext is the foreground equivalent: the VM is tied to the CLI's
// lifetime here, which is exactly what an attached console means.
var vmCommandContext = exec.CommandContext

// StartDetached launches a VM in the background and records the run. The VM
// outlives the CLI: Ctrl-C on the command that started it must not power the
// guest off.
//
// Returns ErrAlreadyRunning if the VM is already up, which callers that only
// need it running should treat as success.
// ErrDetachUnsupported reports that this host cannot run a VM in the
// background. Windows has neither the inherited-descriptor lock nor the signal
// semantics the detached path relies on; foreground `wendy vm start` still
// works. Declared here rather than beside detachProcess so callers can check
// DetachSupported on every platform.
var ErrDetachUnsupported = errors.New("detached VMs need macOS or Linux; use 'wendy vm start'")

func (s *Store) StartDetached(spec Spec) (State, error) {
	if err := ValidName(spec.Name); err != nil {
		return State{}, err
	}

	lock, err := s.acquireRunLock(spec.Name)
	if err != nil {
		return State{}, err
	}
	// The parent always drops its copy: the lock must end up held by QEMU
	// alone, so that QEMU's death is what frees it. Bare, and deliberately so --
	// nothing is written through a lock handle, and a close error here would
	// otherwise report a failure for a VM that started perfectly well.
	defer func() { _ = lock.Close() }()

	// Args before openConsoleLog: it is the last thing that can reject the
	// spec, and rotating the log first would discard the previous boot's
	// output over a launch that never happens.
	spec.ConsoleLog = s.LogPath(spec.Name)
	if DetachSupported() {
		spec.QMPPath, err = s.prepareQMP(spec.Name)
		if err != nil {
			return State{}, err
		}
	}
	args, err := spec.Args()
	if err != nil {
		return State{}, err
	}

	logFile, err := s.openConsoleLog(spec.Name)
	if err != nil {
		return State{}, err
	}
	// Closed bare: this handle is handed to QEMU below and never written to
	// here, and QEMU keeps its own descriptor open long after the parent drops
	// this one.
	defer func() { _ = logFile.Close() }()

	// Recorded before the process exists so a concurrent reader never sees a
	// held lock with no state at all. PID 0 reads as "starting".
	st := State{
		Name:      spec.Name,
		AgentPort: spec.Net.AgentPort,
		NetMode:   spec.Net.Mode,
		MAC:       spec.Net.MAC,
		Accel:     spec.Accel,
		MemoryMiB: spec.MemoryMiB,
		CPUs:      spec.CPUs,
		StartedAt: time.Now().UTC(),
		Detached:  true,
		LogPath:   spec.ConsoleLog,
	}
	if err := s.WriteState(st); err != nil {
		return State{}, err
	}

	cmd := vmCommand(spec.Binary(), args...)
	cmd.Stdin = nil
	// QEMU's own startup failures ("Could not set up host forwarding rule")
	// belong in the same log as the guest console; they are all a user has
	// when a detached boot fails.
	cmd.Stdout, cmd.Stderr = logFile, logFile
	attachRunLock(cmd, lock)
	if err := detachProcess(cmd); err != nil {
		_ = os.Remove(s.StatePath(spec.Name))
		return State{}, err
	}

	if err := cmd.Start(); err != nil {
		_ = os.Remove(s.StatePath(spec.Name))
		return State{}, fmt.Errorf("starting %s: %w", spec.Binary(), err)
	}

	st.PID = cmd.Process.Pid
	if err := s.WriteState(st); err != nil {
		// The record still says pid 0, which every command reads as "starting"
		// and refuses to act on. Rather than leave a VM nothing can stop, take
		// it down with us.
		_ = killProcess(st.PID, true)
		_ = os.Remove(s.StatePath(spec.Name))
		return State{}, err
	}

	// Reap the child if this process outlives it, and clear the record so a
	// long-lived CLI notices a VM that died mid-deploy. When the CLI exits
	// first the VM is simply orphaned to init, which is the intent.
	go func() {
		_ = cmd.Wait()
		s.clearStateForPID(spec.Name, st.PID)
	}()

	return st, nil
}

// clearStateForPID removes the run record only while it still describes pid.
// A second run may have started and written its own record by the time this
// process reaps its child, and deleting that would report the live VM as
// merely "starting", with no address.
func (s *Store) clearStateForPID(name string, pid int) {
	// A PID comparison alone races a new start between ReadState and Remove.
	// Serialize both operations against writers using the same run lock.
	f, err := os.OpenFile(s.LockPath(name), os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	locked, err := tryLockFile(f)
	if err != nil || !locked {
		return
	}
	defer unlockFile(f)
	s.clearStateForPIDLocked(name, pid)
}

// The caller owns the exclusive run lock, including through Remove.
func (s *Store) clearStateForPIDLocked(name string, pid int) {
	if cur, err := s.ReadState(name); err != nil || cur.PID != pid {
		return
	}
	_ = os.Remove(s.StatePath(name))
}

// openConsoleLog truncates a fresh log, keeping the previous run's alongside it
// so a boot failure is still diagnosable after one retry.
func (s *Store) openConsoleLog(name string) (*os.File, error) {
	path := s.LogPath(name)
	if err := os.Rename(path, path+".prev"); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening console log: %w", err)
	}
	return f, nil
}

// Stop shuts a running VM down.
//
// Waits on the run lock rather than the pid: the kernel frees it when the
// emulator's last descriptor closes, so this reports the process is really
// gone rather than that a signal was delivered.
//
// Without force, request the guest's power-button handler through QMP. A
// missing monitor or an unresponsive guest is an error, never permission to
// cut power. Only an explicit force request may kill the emulator.
func (s *Store) Stop(name string, force bool, grace time.Duration) error {
	return s.StopContext(context.Background(), name, force, grace)
}

// StopContext lets callers abandon the wait without cutting the VM's power.
// A shutdown already requested from the guest continues after cancellation.
func (s *Store) StopContext(ctx context.Context, name string, force bool, grace time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.Status(name)
	if err != nil {
		return err
	}
	if !st.Running {
		return nil
	}
	// Running with no pid means the record was written but the emulator has not
	// been started yet, or the record is unreadable. Either way there is nothing
	// safe to signal, and reporting success would let `vm rm --force` delete the
	// disk out from under a live emulator.
	if st.State.PID == 0 {
		return fmt.Errorf("VM %q is starting; retry in a moment", name)
	}

	if force {
		if err := killProcess(st.State.PID, true); err != nil {
			return fmt.Errorf("signalling VM %q: %w", name, err)
		}
		grace = time.Second
	} else if err := s.requestPowerdown(ctx, name); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("requesting VM %q shutdown: %w; shut down inside the guest or use 'wendy vm stop %s --force' to cut power", name, err, name)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		held, err := s.runLockHeld(name)
		if err != nil {
			return err
		}
		if !held {
			_, err := s.Status(name) // reaps state.json
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if force {
		return fmt.Errorf("VM %q did not exit after SIGKILL", name)
	}
	return fmt.Errorf("VM %q has not shut down after %s; it was not killed: wait longer or use 'wendy vm stop %s --force' to cut power", name, grace, name)
}

// RunForeground runs a VM attached to the given streams and blocks until it
// exits, recording the run exactly as StartDetached does.
//
// The record is what `wendy vm list`, discovery and the simulator picker read,
// so leaving it out would make a VM started this way invisible to all three --
// and would let them try to start a second emulator on the same disk.
func (s *Store) RunForeground(ctx context.Context, spec Spec, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ValidName(spec.Name); err != nil {
		return err
	}
	lock, err := s.acquireRunLock(spec.Name)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	// No ConsoleLog: the console is this terminal, which is the whole point of
	// a foreground start.
	if DetachSupported() {
		spec.QMPPath, err = s.prepareQMP(spec.Name)
		if err != nil {
			return err
		}
	}
	args, err := spec.Args()
	if err != nil {
		return err
	}

	st := State{
		Name:      spec.Name,
		AgentPort: spec.Net.AgentPort,
		NetMode:   spec.Net.Mode,
		MAC:       spec.Net.MAC,
		Accel:     spec.Accel,
		MemoryMiB: spec.MemoryMiB,
		CPUs:      spec.CPUs,
		StartedAt: time.Now().UTC(),
	}
	if err := s.WriteState(st); err != nil {
		return err
	}
	// Captured after Start; until then the record has pid 0 and clearing it is
	// still correct, because nothing else can have taken the lock.
	startedPID := 0
	// This foreground launcher still owns lock until its earlier Close defer.
	defer func() { s.clearStateForPIDLocked(spec.Name, startedPID) }()

	qemu := vmCommandContext(ctx, spec.Binary(), args...)
	qemu.Stdin, qemu.Stdout, qemu.Stderr = stdin, stdout, stderr
	// Where the platform allows it the lock rides on the emulator's descriptor,
	// so a CLI killed outright does not free it while its VM still runs.
	attachRunLock(qemu, lock)
	qemu.Cancel = func() error { return terminateProcess(qemu.Process) }
	if err := qemu.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", spec.Binary(), err)
	}

	st.PID = qemu.Process.Pid
	startedPID = st.PID
	if err := s.WriteState(st); err != nil {
		// QEMU is up and holding the lock, but the record still says pid 0, so
		// no command would act on it. Take it down rather than orphan it --
		// StartDetached does the same with the identical failure.
		_ = killProcess(st.PID, true)
		_ = qemu.Wait()
		return err
	}
	return qemu.Wait()
}
