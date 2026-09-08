//go:build darwin || linux

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubEmulator points vmCommand at a long-lived process instead of QEMU.
//
// It execs the binary directly rather than going through a shell: a shell
// forks a grandchild that also inherits the run-lock descriptor, so the lock
// would outlive the process the test kills and liveness would look stuck.
func stubEmulator(t *testing.T) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	savedCmd, savedCtx := vmCommand, vmCommandContext
	vmCommand = func(_ string, _ ...string) *exec.Cmd { return exec.Command(sleep, "30") }
	vmCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, sleep, "30")
	}
	t.Cleanup(func() { vmCommand, vmCommandContext = savedCmd, savedCtx })
}

func detachableSpec(t *testing.T, s *Store, name string) Spec {
	t.Helper()
	createTestVM(t, s, name, Meta{})
	return Spec{
		Name:         name,
		DiskPath:     s.DiskPath(name),
		FirmwareCode: filepath.Join(t.TempDir(), "code.fd"),
		FirmwareVars: s.VarsPath(name),
		Accel:        AccelTCG,
		Net:          NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor(name)},
	}
}

func waitForStop(t *testing.T, s *Store, name string) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := s.Status(name)
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if !st.Running || time.Now().After(deadline) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartDetachedRecordsTheRun(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")

	st, err := s.StartDetached(spec)
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}
	t.Cleanup(func() {
		if st.PID > 0 {
			_ = syscallKill(st.PID)
		}
	})

	if st.PID <= 0 {
		t.Errorf("StartDetached().PID = %d, want the real pid", st.PID)
	}
	if !st.Detached {
		t.Error("StartDetached() recorded Detached = false")
	}

	got, err := s.Status("dev")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if !got.Running || got.State.PID != st.PID {
		t.Errorf("Status() = %+v, want running with pid %d", got, st.PID)
	}
}

func TestStartDetachedCreatesAnOwnerOnlyConsoleLog(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	st, err := s.StartDetached(detachableSpec(t, s, "dev"))
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}
	t.Cleanup(func() { _ = syscallKill(st.PID) })

	info, err := os.Stat(s.LogPath("dev"))
	if err != nil {
		t.Fatalf("stat console log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("console.log mode = %v, want 0600", perm)
	}
}

func TestStartDetachedRefusesASecondInstance(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")

	st, err := s.StartDetached(spec)
	if err != nil {
		t.Fatalf("first StartDetached() = %v", err)
	}
	t.Cleanup(func() { _ = syscallKill(st.PID) })

	if _, err := s.StartDetached(spec); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second StartDetached() = %v, want ErrAlreadyRunning", err)
	}
}

func TestKillingTheVMFreesTheLockAndClearsTheState(t *testing.T) {
	// The whole point of holding the lock on the emulator's own descriptor:
	// a hard kill leaves no chance to clean up, and the kernel does it for us.
	stubEmulator(t)
	s := newTestStore(t)
	st, err := s.StartDetached(detachableSpec(t, s, "dev"))
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}
	if err := syscallKill(st.PID); err != nil {
		t.Fatalf("kill: %v", err)
	}

	got := waitForStop(t, s, "dev")
	if got.Running {
		t.Error("Status() still reports running after the VM was killed")
	}
	if _, err := os.Stat(s.StatePath("dev")); !os.IsNotExist(err) {
		t.Errorf("state.json survived the kill: %v", err)
	}
}

func TestStartDetachedKeepsThePreviousConsoleLog(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")

	first, err := s.StartDetached(spec)
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}
	if err := os.WriteFile(s.LogPath("dev"), []byte("first boot"), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	_ = syscallKill(first.PID)
	waitForStop(t, s, "dev")

	second, err := s.StartDetached(spec)
	if err != nil {
		t.Fatalf("second StartDetached() = %v", err)
	}
	t.Cleanup(func() { _ = syscallKill(second.PID) })

	prev, err := os.ReadFile(s.LogPath("dev") + ".prev")
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if !strings.Contains(string(prev), "first boot") {
		t.Errorf("console.log.prev = %q, want the previous run's output", prev)
	}
}

func TestStopReturnsOnlyOnceTheVMIsReallyGone(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	st, err := s.StartDetached(detachableSpec(t, s, "dev"))
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}

	if err := s.Stop("dev", true, 5*time.Second); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	got, err := s.Status("dev")
	if err != nil {
		t.Fatalf("Status() = %v", err)
	}
	if got.Running {
		t.Errorf("Status() after Stop() = %+v, want stopped", got)
	}
	if _, err := os.Stat(s.StatePath("dev")); !os.IsNotExist(err) {
		t.Errorf("state.json survived Stop(): %v", err)
	}
	_ = st
}

func TestStopIsANoOpOnAStoppedVM(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	if err := s.Stop("dev", false, time.Second); err != nil {
		t.Errorf("Stop() on a stopped VM = %v, want nil", err)
	}
}

func TestGracefulStopNeverSilentlyCutsPower(t *testing.T) {
	for _, mode := range []string{"missing", "refused", "timeout", "shutdown", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			stubEmulator(t)
			s := shortQMPStore(t)
			st, err := s.StartDetached(detachableSpec(t, s, "dev"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop("dev", true, time.Second) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode != "missing" {
				ln, err := net.Listen("unix", s.QMPPath("dev"))
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				go func() {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(time.Second))
					e, d := json.NewEncoder(c), json.NewDecoder(c)
					_ = e.Encode(map[string]any{"QMP": map[string]any{}})
					for _, want := range []string{"qmp_capabilities", "system_powerdown"} {
						var req struct{ Execute string }
						if err := d.Decode(&req); err != nil {
							return
						}
						if req.Execute != want {
							t.Errorf("command = %s, want %s", req.Execute, want)
							return
						}
						if want == "system_powerdown" && mode == "refused" {
							_ = e.Encode(map[string]any{"error": map[string]string{"desc": "denied"}})
							return
						}
						_ = e.Encode(map[string]any{"return": map[string]any{}})
					}
					if mode == "shutdown" {
						_ = killProcess(st.PID, true)
					} // emulate guest exit
					if mode == "cancelled" {
						cancel()
					}
				}()
			}
			err = s.StopContext(ctx, "dev", false, 200*time.Millisecond)
			if mode == "shutdown" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("want cancellation, got %v", err)
				}
				if mode != "cancelled" && (err == nil || !strings.Contains(err.Error(), "--force")) {
					t.Fatalf("want explicit-force guidance, got %v", err)
				}
				status, err := s.Status("dev")
				if err != nil || !status.Running {
					t.Fatalf("guest was killed: %+v, %v", status, err)
				}
			}
		})
	}
}

func TestForegroundRunIsVisibleLikeADetachedOne(t *testing.T) {
	// A VM started in the foreground must still be discoverable: `vm list`,
	// device discovery and the simulator picker all read the run record, and
	// without one they would report it stopped and try to start a second
	// emulator on the same disk.
	stubEmulator(t)
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunForeground(ctx, spec, nil, io.Discard, io.Discard) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := s.Status("dev")
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if st.Running && st.State.PID > 0 {
			if st.State.Detached {
				t.Error("a foreground run recorded Detached = true")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground run never became visible in Status()")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done
	got := waitForStop(t, s, "dev")
	if got.Running {
		t.Error("Status() still reports running after the foreground run ended")
	}
}

func TestForegroundRunRefusesASecondInstance(t *testing.T) {
	stubEmulator(t)
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")

	st, err := s.StartDetached(spec)
	if err != nil {
		t.Fatalf("StartDetached() = %v", err)
	}
	t.Cleanup(func() { _ = syscallKill(st.PID) })

	err = s.RunForeground(context.Background(), spec, nil, io.Discard, io.Discard)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("RunForeground() on a running VM = %v, want ErrAlreadyRunning", err)
	}
}

func TestStopRefusesAVMWithNoRecordedPID(t *testing.T) {
	// Running with no pid means the emulator has not started yet, or the record
	// is unreadable. Reporting success here let `vm rm --force` delete the disk
	// out from under a live emulator.
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	lock, err := s.acquireRunLock("dev")
	if err != nil {
		t.Fatalf("acquireRunLock() = %v", err)
	}
	defer lock.Close()
	if err := s.WriteState(State{Name: "dev"}); err != nil {
		t.Fatalf("WriteState() = %v", err)
	}

	if err := s.Stop("dev", true, time.Second); err == nil {
		t.Error("Stop() = nil for a VM with no recorded pid, want a refusal")
	}
}

func TestARejectedSpecKeepsThePreviousConsoleLog(t *testing.T) {
	// Args is the last thing that can reject a spec. Rotating the console log
	// before it runs would discard the previous boot's output -- exactly what
	// the user goes looking for when a start stops working.
	s := newTestStore(t)
	spec := detachableSpec(t, s, "dev")
	spec.Net.Mode = "bridged"

	const previous = "earlier boot output\n"
	if err := os.WriteFile(s.LogPath("dev"), []byte(previous), 0o600); err != nil {
		t.Fatalf("seed console log: %v", err)
	}

	if _, err := s.StartDetached(spec); err == nil {
		t.Fatal("StartDetached() accepted an unknown network mode")
	}
	got, err := os.ReadFile(s.LogPath("dev"))
	if err != nil {
		t.Fatalf("read console log: %v", err)
	}
	if string(got) != previous {
		t.Errorf("console.log = %q, want the previous boot's output untouched", got)
	}
}
