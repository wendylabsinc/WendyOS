//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type harness struct {
	t       *testing.T
	sup     *Supervisor
	store   Store
	probe   atomic.Value // error
	alive   atomic.Value // error
	pings   atomic.Int32 // answered pings
	reloads atomic.Int32 // NetworkManager reloads
	cancel  context.CancelFunc
	done    chan error
}

var bootStart = time.Now()

func newHarness(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("supervises real child processes")
	}
	t.Setenv("SUPERVISOR_TEST_CHILD", "1")
	dir := t.TempDir()
	image, _ := agentVariant(t, filepath.Join(dir, "image"), "image")
	if err := os.Rename(image, filepath.Join(dir, "image", "wendy-agent")); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: Store{Dir: filepath.Join(dir, "data")}}
	h.probe.Store(errNone)
	h.alive.Store(errNone)
	h.sup = &Supervisor{
		cfg: Config{
			Store:           h.store,
			ImageAgent:      filepath.Join(dir, "image", "wendy-agent"),
			ImageVersion:    "2026.10.29-210857",
			Arch:            runtime.GOARCH,
			StatusPath:      filepath.Join(dir, "run", "status.json"),
			SnapshotDirs:    []SnapshotDir{{Path: filepath.Join(dir, "etc-wendy-agent")}},
			TrialDeadline:   2 * time.Second,
			MinTrialUptime:  150 * time.Millisecond,
			ProbeEvery:      20 * time.Millisecond,
			LivenessEvery:   20 * time.Millisecond,
			LivenessMisses:  3,
			GraceMissWindow: 60 * time.Millisecond,
			RestartDelay:    10 * time.Millisecond,
			StopTimeout:     2 * time.Second,
		},
		deps: Deps{
			DataMounted: func() bool { return true },
			Emergency:   func() bool { return false },
			BootID:      func() string { return "boot-1" },
			Uptime:      func() time.Duration { return time.Since(bootStart) },
			Build:       func() string { return "build-1" },
			RunVersion:  printsVersion("2026.11.02-101010"),
			OSGate:      func(context.Context) error { return nil },
			Probe:       func(context.Context, int, string) error { return unwrap(h.probe.Load()) },
			Alive: func(context.Context) error {
				err := unwrap(h.alive.Load())
				if err == nil {
					h.pings.Add(1)
				}
				return err
			},
			RemoteSeen: func(int, string) bool { return true },
			Serving:    func(int) bool { return false },
			ReloadNetwork: func() error {
				h.reloads.Add(1)
				return nil
			},
			Log: zap.NewNop(),
		},
	}
	return h
}

var errNone = errors.New("none")

func unwrap(v any) error {
	if err := v.(error); err != errNone {
		return err
	}
	return nil
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.sup.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("Run did not return after cancel")
	}
	h.cancel = nil
}

func (h *harness) state() State {
	st, err := LoadState(h.store.StatePath())
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for: %s", what)
}

func (h *harness) status() Status {
	st, _ := ReadStatus(h.sup.cfg.StatusPath)
	return st
}

func TestRunStartsTheImageAgentWithoutAnUpdate(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().Role == RoleImage && h.status().PID > 0 })
}

func TestRunApprovesATrialThatPassesItsChecks(t *testing.T) {
	h := newHarness(t)
	sum := dropInInbox(t, h.store, "new", Incoming{RemoteVia: "lan", Explicit: true})
	h.start()
	h.eventually("the new agent is approved", func() bool {
		st := h.state()
		return st.Approved != nil && st.Approved.SHA256 == sum && st.Trial == nil
	})
	if got := h.state().Approved.Builds; len(got) != 1 || got[0] != "build-1" {
		t.Fatalf("the agent must be validated for this build, got %v", got)
	}
	h.eventually("the status says approved", func() bool {
		s := h.status()
		return s.Role == RoleApproved && s.UpdateState == UpdateApproved && s.UpdateSHA256 == sum
	})
}

func TestRunApprovesOnlyAfterTheMinimumUptime(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.MinTrialUptime = 600 * time.Millisecond
	dropInInbox(t, h.store, "new", Incoming{Explicit: true})
	h.start()
	h.eventually("the trial runs", func() bool { return h.status().Role == RoleTrial && h.status().PID > 0 })
	time.Sleep(200 * time.Millisecond)
	if h.state().Approved != nil {
		t.Fatal("a trial must stay up for the minimum uptime before it is approved")
	}
	h.eventually("the trial is approved", func() bool { return h.state().Approved != nil })
}

func TestRunRejectsATrialThatMissesItsDeadlineAndRestoresTheSnapshot(t *testing.T) {
	h := newHarness(t)
	h.probe.Store(errors.New("no listener on 50052"))
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	h.sup.deps.Probe = func(context.Context, int, string) error {
		os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
		return unwrap(h.probe.Load())
	}
	sum := dropInInbox(t, h.store, "bad", Incoming{Explicit: true})
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("the image agent runs again", func() bool { return h.status().Role == RoleImage })

	got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json"))
	if string(got) != "before" {
		t.Fatalf("the config must be restored, got %q", got)
	}
	if s := h.status(); s.UpdateState != UpdateRejected || s.Reason == "" || s.UpdateSHA256 != sum {
		t.Fatalf("status must say why and about which agent, got %+v", s)
	}
	// Clients print it after "the new agent failed its checks: ".
	if s := h.status(); !strings.HasPrefix(s.Reason, "did not pass its checks within ") {
		t.Fatalf("reason = %q", s.Reason)
	}
}

func TestRunRequiresTheRemoteConfirmWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.RemoteSeen = func(int, string) bool { return false }
	sum := dropInInbox(t, h.store, "lonely", Incoming{RemoteVia: "lan", Explicit: true})
	h.start()
	h.eventually("the unconfirmed trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
}

func TestRunRejectsATrialThatKeepsCrashing(t *testing.T) {
	h := newHarness(t)
	// A crashing agent never passes the real probe; a long deadline leaves the
	// crash budget as the only way to reject it.
	h.probe.Store(errors.New("not listening"))
	h.sup.cfg.TrialDeadline = time.Minute
	sum := dropInInbox(t, h.store, "crashy", Incoming{Explicit: true})
	t.Setenv(childBehaviorEnv(sum), "crash")
	h.start()
	h.eventually("the crashing trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	if r := h.state().Rejected[0].Reason; !strings.Contains(r, "during its trial") {
		t.Fatalf("want the crash budget to reject it, got %q", r)
	}
	h.eventually("the image agent runs", func() bool { return h.status().Role == RoleImage })
}

func TestRunFallsBackWhenTheApprovedAgentKeepsCrashing(t *testing.T) {
	h := newHarness(t)
	_, sum := agentVariant(t, filepath.Join(h.store.Dir, "bin"), "approved")
	if err := SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"build-1"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(childBehaviorEnv(sum), "crash")
	h.start()
	h.eventually("the image agent takes over", func() bool { return h.status().Role == RoleImage })
	if h.state().Approved == nil {
		t.Fatal("the approved agent is only skipped for this boot")
	}
}

func TestRunRestartsAnAgentThatStopsAnswering(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.eventually("the image agent answered once", func() bool { return h.status().PID > 0 && h.pings.Load() > 0 })
	first := h.status().PID
	h.alive.Store(errors.New("deadline exceeded"))
	h.eventually("a new agent process", func() bool { p := h.status().PID; return p > 0 && p != first })
}

func TestRunSparesTheImageAgentWhileItsOSGateRuns(t *testing.T) {
	h := newHarness(t)
	// It opens its socket only once its own OS gate is through.
	h.alive.Store(errors.New("connection refused"))
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().PID > 0 })
	first := h.status().PID
	time.Sleep(10 * h.sup.cfg.LivenessEvery)
	if p := h.status().PID; p != first {
		t.Fatalf("the image agent was restarted inside its OS gate (%d -> %d)", first, p)
	}
}

func TestRunKillsWhatTheAgentLeftBehind(t *testing.T) {
	h := newHarness(t)
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	t.Setenv("SUPERVISOR_TEST_ORPHAN_PID", pidFile)
	t.Setenv(childBehaviorEnv("wendy-agent"), "orphan")
	h.start()
	var orphan int
	h.eventually("the agent started a helper", func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		orphan, _ = strconv.Atoi(string(data))
		return orphan > 0
	})
	h.eventually("the helper is gone", func() bool { return processAlive(orphan) != nil })
}

func TestRunKillsWhatTheAgentLeftOutsideItsGroup(t *testing.T) {
	h := newHarness(t)
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	t.Setenv("SUPERVISOR_TEST_ORPHAN_PID", pidFile)
	t.Setenv(childBehaviorEnv("wendy-agent"), "setsid-orphan")
	readPid := func() int {
		data, _ := os.ReadFile(pidFile)
		pid, _ := strconv.Atoi(string(data))
		return pid
	}
	// The unit's cgroup holds the helper and the supervisor itself.
	h.sup.deps.UnitProcs = func() []int {
		procs := []int{os.Getpid()}
		if pid := readPid(); pid > 0 && running(pid) {
			procs = append(procs, pid)
		}
		return procs
	}
	h.start()
	var orphan int
	h.eventually("the agent started a helper", func() bool { orphan = readPid(); return orphan > 0 })
	t.Cleanup(func() { syscall.Kill(orphan, syscall.SIGKILL) })
	h.eventually("the helper is gone", func() bool { return !running(orphan) })
}

// running reports whether pid runs; a zombie waiting to be reaped does not count.
func running(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(after, "Z")
}

func TestUnitProcsListsThisProcess(t *testing.T) {
	procs := UnitProcs()
	if procs == nil {
		t.Skip("no cgroup v2 here")
	}
	if !slices.Contains(procs, os.Getpid()) {
		t.Fatalf("own cgroup %v lacks pid %d", procs, os.Getpid())
	}
}

func TestRunRestartsAfterACleanExit(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	t.Setenv(childBehaviorEnv(sum), "clean")
	h.start()
	h.eventually("the approved agent runs", func() bool { return h.status().Role == RoleApproved && h.status().PID > 0 })
	first := h.status().PID
	h.eventually("it was restarted", func() bool { p := h.status().PID; return p > 0 && p != first })
	if n := h.state().FailureCount(sum, "boot-1"); n != 0 {
		t.Fatalf("clean exits must not count, got %d", n)
	}
	if r := h.status().Role; r != RoleApproved {
		t.Fatalf("clean restarts must keep the approved agent, got %s", r)
	}
}

// approveVariant installs an approved agent for build-1.
func approveVariant(t *testing.T, h *harness, tag string) string {
	t.Helper()
	_, sum := agentVariant(t, filepath.Join(h.store.Dir, "bin"), tag)
	if err := SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"build-1"}}}); err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestRunRejectsATrialThatOnlyExitsCleanly(t *testing.T) {
	h := newHarness(t)
	// An agent that keeps exiting never passes the real probe.
	h.probe.Store(errors.New("not listening"))
	h.sup.cfg.TrialDeadline = 500 * time.Millisecond
	sum := dropInInbox(t, h.store, "quitter", Incoming{Explicit: true})
	t.Setenv(childBehaviorEnv(sum), "clean")
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("the image agent runs", func() bool { return h.status().Role == RoleImage })
}

func TestRunKeepsAnExplicitlyRetriedAgentOnceApproved(t *testing.T) {
	h := newHarness(t)
	sum := dropInInbox(t, h.store, "retried", Incoming{Explicit: true})
	if err := SaveState(h.store.StatePath(), State{Rejected: []Rejection{{SHA256: sum, Build: "build-1", Reason: "the client went away"}}}); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.eventually("the retry is approved", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
	if h.state().IsRejected(sum, "build-1") {
		t.Fatal("approving an agent must clear its old rejection on this build")
	}
}

func TestRunRestoresTheConfigWhenTheCrashBudgetRejectsATrial(t *testing.T) {
	h := newHarness(t)
	h.probe.Store(errors.New("not listening"))
	h.sup.cfg.TrialDeadline = time.Minute
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	sum := dropInInbox(t, h.store, "crashy", Incoming{Explicit: true})
	t.Setenv(childBehaviorEnv(sum), "crash")
	h.sup.deps.Probe = func(context.Context, int, string) error {
		return errors.New("not listening")
	}
	// The trial's agent rewrites the config before it crashes.
	go func() {
		for i := 0; i < 200; i++ {
			if h.status().Role == RoleTrial {
				os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("the config is restored", func() bool {
		got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json"))
		return string(got) == "before"
	})
	h.eventually("the snapshot is gone", func() bool {
		entries, _ := os.ReadDir(h.store.SnapshotDir())
		return len(entries) == 0
	})
}

func TestRunRunsTheImageAgentFirstWhenTheOSGateFails(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	// The image agent keeps running, as a real one does once it answers.
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	var roles []Role
	h.start()
	h.eventually("the approved agent runs after the image agent", func() bool {
		s := h.status()
		if s.PID > 0 && (len(roles) == 0 || roles[len(roles)-1] != s.Role) {
			roles = append(roles, s.Role)
		}
		return s.Role == RoleApproved && s.SHA256 == sum
	})
	if roles[0] != RoleImage {
		t.Fatalf("the image agent must run the OS gate first, roles seen: %v", roles)
	}
}

func TestRunPausesUpdatesWhenItCannotTakeThem(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.Emergency = func() bool { return true }
	h.start()
	h.eventually("the image agent runs with updates paused", func() bool {
		s := h.status()
		return s.Role == RoleImage && s.UpdatesPaused != ""
	})
}

func TestRunSetsACorruptStateAside(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(h.store.Dir, 0o755)
	os.WriteFile(h.store.StatePath(), []byte("{garbage"), 0o644)
	sum := dropInInbox(t, h.store, "new", Incoming{Explicit: true})
	h.start()
	h.eventually("the corrupt state is set aside", func() bool {
		_, err := os.Stat(h.store.StatePath() + ".corrupt")
		return err == nil
	})
	h.eventually("updates work again", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
}

func TestRunMarksARevalidationInTheStatus(t *testing.T) {
	h := newHarness(t)
	_, sum := agentVariant(t, filepath.Join(h.store.Dir, "bin"), "approved")
	SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"old-build"}}})
	h.sup.cfg.MinTrialUptime = 300 * time.Millisecond
	h.start()
	h.eventually("the revalidation runs", func() bool { s := h.status(); return s.Role == RoleTrial && s.Revalidation })
	h.eventually("it is approved for this build", func() bool {
		st := h.state()
		return st.Approved != nil && len(st.Approved.Builds) == 2
	})
}

func TestRunRunsTheImageAgentWhenTheStateCannotBeSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	_, sum := agentVariant(t, filepath.Join(h.store.Dir, "bin"), "trial")
	os.MkdirAll(h.store.InboxDir(), 0o755)
	if err := SaveState(h.store.StatePath(), State{Trial: trial(sum)}); err != nil {
		t.Fatal(err)
	}
	// A read-only store: the failure count could not be kept, so no trial may run.
	if err := os.Chmod(h.store.Dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h.store.Dir, 0o755) })
	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().Role == RoleImage && h.status().PID > 0 })
}

func processAlive(pid int) error {
	return syscall.Kill(pid, 0)
}

func TestRunStopsTheAgentWhenCancelled(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().PID > 0 })
	pid := h.status().PID
	h.stop()
	if err := processAlive(pid); err == nil {
		t.Fatalf("pid %d is still running after Run returned", pid)
	}
}

func TestRunPausesUpdatesUntilTheImageAgentHasCheckedTheOS(t *testing.T) {
	h := newHarness(t)
	h.alive.Store(errors.New("not up yet"))
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	h.start()
	h.eventually("updates are paused", func() bool {
		s := h.status()
		return s.Role == RoleImage && s.UpdatesPaused != ""
	})
}

func TestRunRetriesAFailedRestoreAndRestartsTheAgent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	// A rejected trial left its snapshot behind, but the config cannot be written yet.
	snap := filepath.Join(h.store.SnapshotDir(), "rejected-sha")
	if err := snapshot(snap, h.sup.cfg.SnapshotDirs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
	os.Chmod(cfgDir, 0o500)
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) })

	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().PID > 0 })
	first := h.status().PID
	os.Chmod(cfgDir, 0o700)
	h.eventually("the config is restored", func() bool {
		got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json"))
		return string(got) == "before"
	})
	h.eventually("the agent restarted on the restored config", func() bool { p := h.status().PID; return p > 0 && p != first })
}

func TestRunDropsADamagedApprovedAgentWithoutGivingUpOnIt(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	if err := os.WriteFile(h.store.BinPath(sum), []byte("damaged on disk"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	// The file failed, not the agent: the daily update may install it again.
	if st := h.state(); st.Approved != nil || st.GivenUp(sum, "2026.11.02-101010", "build-1") {
		t.Fatalf("state = %+v", st)
	}
}

func TestRunKeepsTheApprovedAgentWhenTheGateCouldNotConfirmTheOSUpdate(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	// Approved on the previous build; this build's image agent is newer.
	SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: sum, Version: "2026.10.01-000000", Builds: []string{"build-0"}}})
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	h.start()
	h.eventually("the image agent runs after the handover", func() bool {
		s := h.status()
		return s.Role == RoleImage && s.PID > 0 && strings.Contains(s.Reason, "same or newer")
	})
	if st := h.state(); st.Approved == nil || st.Approved.SHA256 != sum {
		t.Fatalf("a revert to build-0 must still find its agent: %+v", st.Approved)
	}
	if _, err := os.Stat(h.store.BinPath(sum)); err != nil {
		t.Fatalf("its file must stay: %v", err)
	}
}

func TestRunHoldsTheGateHandoverWhileTheAgentApplies(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	var applying atomic.Bool
	applying.Store(true)
	h.sup.deps.Updating = func(int) bool { return applying.Load() }
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	pid := h.status().PID
	// A restart now would cut off the install it runs.
	time.Sleep(20 * h.sup.cfg.LivenessEvery)
	if s := h.status(); s.PID != pid || s.Role != RoleImage {
		t.Fatalf("the handover ran during an update: %+v", s)
	}
	applying.Store(false)
	h.eventually("the approved agent takes over", func() bool { s := h.status(); return s.Role == RoleApproved && s.SHA256 == sum })
}

func TestRunHoldsTheRestoreRetryWhileTheAgentApplies(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	if err := snapshot(filepath.Join(h.store.SnapshotDir(), "rejected-sha"), h.sup.cfg.SnapshotDirs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
	os.Chmod(cfgDir, 0o500)
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) })
	var applying atomic.Bool
	applying.Store(true)
	h.sup.deps.Updating = func(int) bool { return applying.Load() }

	h.start()
	h.eventually("the image agent runs", func() bool { return h.status().PID > 0 })
	first := h.status().PID
	os.Chmod(cfgDir, 0o700)
	time.Sleep(20 * h.sup.cfg.ProbeEvery)
	if p := h.status().PID; p != first {
		t.Fatalf("the restore retry restarted the agent during an update: %d -> %d", first, p)
	}
	applying.Store(false)
	h.eventually("the config is restored", func() bool {
		got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json"))
		return string(got) == "before"
	})
	h.eventually("the agent restarted on the restored config", func() bool { p := h.status().PID; return p > 0 && p != first })
}

func TestRunHandsOverAfterTheGateEvenWithoutALocalSocket(t *testing.T) {
	h := newHarness(t)
	sum := approveVariant(t, h, "approved")
	h.alive.Store(errors.New("local socket is off"))
	h.sup.deps.Serving = func(int) bool { return true }
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	h.start()
	h.eventually("the approved agent takes over", func() bool { s := h.status(); return s.Role == RoleApproved && s.SHA256 == sum })
}

func TestRunKeepsTheEmergencyPauseThroughTheGateHandover(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.Emergency = func() bool { return true }
	h.sup.deps.OSGate = func(context.Context) error { return errors.New("timed out") }
	h.start()
	h.eventually("the image agent answered", func() bool { return h.pings.Load() > 0 })
	time.Sleep(5 * h.sup.cfg.LivenessEvery)
	if s := h.status(); s.UpdatesPaused == "" || !strings.Contains(s.UpdatesPaused, "emergency") {
		t.Fatalf("the emergency pause must survive the handover, status %+v", s)
	}
}

func TestRunStartsNoNewTrialWhileARestoreIsPending(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	if err := snapshot(filepath.Join(h.store.SnapshotDir(), "rejected-sha"), h.sup.cfg.SnapshotDirs); err != nil {
		t.Fatal(err)
	}
	os.Chmod(cfgDir, 0o500)
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) })
	sum := dropInInbox(t, h.store, "new", Incoming{Explicit: true})
	h.start()
	h.eventually("updates wait for the restore", func() bool { return strings.Contains(h.status().UpdatesPaused, "restored") })
	if st := h.state(); st.Trial != nil {
		t.Fatalf("no trial may start on an unrestored config, got %+v", st.Trial)
	}
	if _, err := os.Stat(filepath.Join(h.store.InboxDir(), sum+".json")); err != nil {
		t.Fatal("the upload must wait in the inbox")
	}
}

func TestRunReportsARePushOfTheApprovedAgentAsApproved(t *testing.T) {
	h := newHarness(t)
	sum := dropInInbox(t, h.store, "approved", Incoming{Explicit: true})
	os.MkdirAll(filepath.Join(h.store.Dir, "bin"), 0o755)
	data, _ := os.ReadFile(filepath.Join(h.store.InboxDir(), sum))
	os.WriteFile(h.store.BinPath(sum), data, 0o755)
	SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: sum, Version: "2026.11.02-101010", Builds: []string{"build-1"}}})
	h.sup.lastUpdate, h.sup.lastReason = UpdateRejected, "an earlier upload failed"
	h.start()
	h.eventually("the status says approved", func() bool {
		s := h.status()
		return s.Role == RoleApproved && s.UpdateState == UpdateApproved
	})
}

func TestRunReloadsTheNetworkAfterARestore(t *testing.T) {
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "wifi.nmconnection"), []byte("before"), 0o600)
	if err := snapshot(filepath.Join(h.store.SnapshotDir(), "rejected-sha"), h.sup.cfg.SnapshotDirs); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfgDir, "wifi.nmconnection"), []byte("trial wrote this"), 0o600)
	h.start()
	h.eventually("NetworkManager reloads the restored profiles", func() bool { return h.reloads.Load() > 0 })
}

func TestRunPausesUpdatesWhenTheRestoreOfACrashedTrialFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newHarness(t)
	h.probe.Store(errors.New("not listening"))
	h.sup.cfg.TrialDeadline = time.Minute
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	sum := dropInInbox(t, h.store, "crashy", Incoming{Explicit: true})
	t.Setenv(childBehaviorEnv(sum), "crash")
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) })
	go func() {
		for i := 0; i < 400; i++ {
			if h.status().Role == RoleTrial {
				os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
				os.Chmod(cfgDir, 0o500)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("updates wait for the restore", func() bool { return strings.Contains(h.status().UpdatesPaused, "restored") })
	// Its space is freed before the restore is retried: a full disk is the usual cause.
	h.eventually("the rejected binary is removed", func() bool {
		_, err := os.Stat(h.store.BinPath(sum))
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestRunCollectsRejectedBinariesWhileAnApprovedAgentRuns(t *testing.T) {
	h := newHarness(t)
	approveVariant(t, h, "approved")
	h.probe.Store(errors.New("not listening"))
	h.sup.cfg.TrialDeadline = 300 * time.Millisecond
	sum := dropInInbox(t, h.store, "bad", Incoming{Explicit: true})
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("its binary is removed", func() bool {
		_, err := os.Stat(h.store.BinPath(sum))
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestRunWakeStartsAFetchedUpdate(t *testing.T) {
	h := newHarness(t)
	wake := make(chan struct{}, 1)
	h.sup.deps.Wake = wake
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	sum := dropInInbox(t, h.store, "fetched", Incoming{Replaces: ReplacesImage, RemoteVia: ViaCloud, Version: "2026.11.02-101010", Build: "build-1"})
	wake <- struct{}{}
	h.eventually("the fetched agent is approved", func() bool {
		st := h.state()
		return st.Approved != nil && st.Approved.SHA256 == sum
	})
}

func TestRunWakeWithoutAnUploadKeepsTheAgent(t *testing.T) {
	h := newHarness(t)
	wake := make(chan struct{}, 1)
	h.sup.deps.Wake = wake
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	pid := h.status().PID
	wake <- struct{}{}
	time.Sleep(300 * time.Millisecond)
	if got := h.status().PID; got != pid {
		t.Fatalf("a wake with an empty inbox restarted the agent: pid %d -> %d", pid, got)
	}
}

func TestRunWakeDuringATrialWaits(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.MinTrialUptime = 600 * time.Millisecond
	wake := make(chan struct{}, 1)
	h.sup.deps.Wake = wake
	users := dropInInbox(t, h.store, "users", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("the user's trial runs", func() bool { s := h.status(); return s.Role == RoleTrial && s.PID > 0 })
	pid := h.status().PID
	dropInInbox(t, h.store, "fetched", Incoming{Replaces: ReplacesImage, Version: "2026.11.03-000000", Build: "build-1"})
	wake <- struct{}{}
	h.eventually("the user's agent is approved", func() bool {
		st := h.state()
		return st.Approved != nil && st.Approved.SHA256 == users
	})
	if got := h.status().PID; got != pid {
		t.Fatalf("a wake must not stop an agent on trial: pid %d -> %d", pid, got)
	}
}

func TestRunRequiresTheCloudForAnAutomaticUpdate(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.RemoteSeen = func(_ int, via string) bool { return via != ViaCloud }
	sum := dropInInbox(t, h.store, "offline", Incoming{Replaces: ReplacesImage, RemoteVia: ViaCloud, Version: "2026.11.02-101010", Build: "build-1"})
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	st := h.state()
	if r := st.Rejected[len(st.Rejected)-1]; !strings.Contains(r.Reason, "cloud") || r.Version != "2026.11.02-101010" || !r.Unconfirmed {
		t.Fatalf("the rejection must say the cloud was not reached, name the release and allow a retry: %+v", r)
	}
	if st.GivenUp(sum, "", "build-1") {
		t.Fatal("one missed confirm must not stop the retries")
	}
}

func TestRunRejectsAFailingProbeForGood(t *testing.T) {
	h := newHarness(t)
	h.probe.Store(errors.New("no listener on 50052"))
	sum := dropInInbox(t, h.store, "deaf", Incoming{Replaces: ReplacesImage, Version: "2026.11.02-101010", Build: "build-1"})
	h.start()
	h.eventually("the trial is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
	if !h.state().GivenUp(sum, "", "build-1") {
		t.Fatal("an agent that fails its local checks is not retried on this build")
	}
}

func TestRunPublishesAUsersRejectionAndDropsWaitingUpdates(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.RemoteSeen = func(int, string) bool { return false }
	wake := make(chan struct{}, 1)
	h.sup.deps.Wake = wake
	users := dropInInbox(t, h.store, "users", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("the user's trial runs", func() bool { s := h.status(); return s.Role == RoleTrial && s.PID > 0 })
	fetched := dropInInbox(t, h.store, "fetched", Incoming{Replaces: ReplacesImage, Version: "2026.11.02-101010", Build: "build-1"})
	wake <- struct{}{}
	h.eventually("the user's rejection is published", func() bool {
		s := h.status()
		return s.Role == RoleImage && s.UpdateState == UpdateRejected && s.UpdateSHA256 == users
	})
	time.Sleep(300 * time.Millisecond)
	if s := h.status(); s.Role != RoleImage || s.UpdateSHA256 != users {
		t.Fatalf("an automatic update must not start over the user's verdict: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(h.store.InboxDir(), fetched+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an update fetched before the user's trial must be dropped")
	}
}

func TestRunWakeIsIgnoredWhileUpdatesArePausedOrAnUploadRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(h *harness)
		running func(h *harness)
	}{
		{"paused", func(h *harness) { h.sup.deps.Emergency = func() bool { return true } }, func(*harness) {}},
		// The running agent is writing an upload.
		{"an upload is being written", func(*harness) {}, func(h *harness) {
			os.MkdirAll(h.store.InboxDir(), 0o755)
			os.WriteFile(filepath.Join(h.store.InboxDir(), ".agent-update-123"), []byte("half"), 0o600)
		}},
		// A restart would kill the install it runs.
		{"the agent installs an OS update", func(h *harness) {
			h.sup.deps.Updating = func(pid int) bool { return pid == h.status().PID }
		}, func(*harness) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			wake := make(chan struct{}, 1)
			h.sup.deps.Wake = wake
			tc.setup(h)
			h.start()
			h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
			tc.running(h)
			pid := h.status().PID
			dropInInbox(t, h.store, "fetched", Incoming{Replaces: ReplacesImage, Version: "2026.11.02-101010", Build: "build-1"})
			wake <- struct{}{}
			time.Sleep(300 * time.Millisecond)
			if got := h.status().PID; got != pid {
				t.Fatalf("the agent was restarted: pid %d -> %d", pid, got)
			}
		})
	}
}

func TestRunStatusNamesAnAutomaticTrialAndTheAgentsStart(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.MinTrialUptime = 600 * time.Millisecond
	dropInInbox(t, h.store, "fetched", Incoming{Replaces: ReplacesImage, Version: "2026.11.02-101010", Build: "build-1"})
	h.start()
	h.eventually("the automatic trial runs", func() bool { s := h.status(); return s.Role == RoleTrial && s.PID > 0 })
	if s := h.status(); !s.Automatic || s.AgentStarted <= 0 || s.AgentStarted > time.Since(bootStart) {
		t.Fatalf("status = %+v", s)
	}
}

func TestRunRetriesAnAutomaticUpdateThatMissedOnlyItsConfirm(t *testing.T) {
	h := newHarness(t)
	h.sup.deps.RemoteSeen = func(_ int, via string) bool { return via != ViaCloud }
	wake := make(chan struct{}, 1)
	h.sup.deps.Wake = wake
	stage := func() string {
		return dropInInbox(t, h.store, "offline", Incoming{Replaces: ReplacesImage, RemoteVia: ViaCloud, Version: "2026.11.02-101010", Build: "build-1"})
	}
	unconfirmed := func(sha string) int {
		n := 0
		for _, r := range h.state().Rejected {
			if r.SHA256 == sha && r.Unconfirmed {
				n++
			}
		}
		return n
	}
	sum := stage()
	h.start()
	for round := 1; round <= MaxUnconfirmed; round++ {
		if round > 1 {
			stage()
			wake <- struct{}{}
		}
		h.eventually(fmt.Sprintf("trial %d runs and misses its confirm", round), func() bool {
			return unconfirmed(sum) == round && h.status().Role == RoleImage && h.status().PID > 0
		})
	}
	if !h.state().GivenUp(sum, "", "build-1") {
		t.Fatal("after the last retry the release is given up")
	}
	stage()
	wake <- struct{}{}
	h.eventually("the given-up release is refused", func() bool {
		s := h.status()
		return s.UpdateSHA256 == sum && strings.Contains(s.Reason, "already failed its checks")
	})
	time.Sleep(h.sup.cfg.TrialDeadline + 500*time.Millisecond)
	if n := unconfirmed(sum); n != MaxUnconfirmed || h.status().Role == RoleTrial {
		t.Fatalf("no further trial may run, got %d unconfirmed rejections, role %s", n, h.status().Role)
	}
}

func TestRunForgetsTheCloudWaitOnceTheAgentReachesTheCloud(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(h.store.InboxDir(), 0o755)
	os.WriteFile(h.store.CloudDeferralsPath(), []byte("3"), 0o644)
	h.start()
	// The fetch may run only near boot, before the relay is up; the supervisor sees it later.
	h.eventually("the count is cleared", func() bool {
		_, err := os.Stat(h.store.CloudDeferralsPath())
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestRunApprovesEarlyAndKeepsTheFallbackThroughTheGrace(t *testing.T) {
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	sum := dropInInbox(t, h.store, "new", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("the client hears it is approved", func() bool {
		s := h.status()
		return s.UpdateState == UpdateApproved && s.UpdateSHA256 == sum && s.Role == RoleApproved && s.Grace
	})
	if st := h.state(); st.Trial == nil || !st.Trial.Confirmed || st.Approved != nil {
		t.Fatalf("until the grace ends the fallback is kept: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(h.store.SnapshotDir(), sum)); err != nil {
		t.Fatalf("the config snapshot is kept for the grace: %v", err)
	}
	h.eventually("the grace ends", func() bool {
		st := h.state()
		return st.Approved != nil && st.Approved.SHA256 == sum && st.Trial == nil && !h.status().Grace
	})
	if _, err := os.Stat(filepath.Join(h.store.SnapshotDir(), sum)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the snapshot goes once the agent is approved for good")
	}
}

func TestRunRejectsAnAgentThatCrashesDuringItsGrace(t *testing.T) {
	h := newHarness(t)
	// Room for three slow restarts (as under -race) before the grace ends.
	h.sup.cfg.TrialDeadline = 8 * time.Second
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	h.sup.deps.Probe = func(context.Context, int, string) error {
		os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
		return nil
	}
	sum := dropInInbox(t, h.store, "late", Incoming{Explicit: true, RemoteVia: "lan"})
	t.Setenv(childBehaviorEnv(sum), "late-crash")
	h.start()
	h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
	h.eventually("its crashes reject it", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("the image agent runs again", func() bool { return h.status().Role == RoleImage })
	if got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json")); string(got) != "before" {
		t.Fatalf("the config must be restored, got %q", got)
	}
	if s := h.status(); s.UpdateState != UpdateRejected || s.UpdateSHA256 != sum {
		t.Fatalf("status = %+v", s)
	}
}

func TestRunJudgesARestartInTheGrace(t *testing.T) {
	for _, answers := range []bool{true, false} {
		t.Run(fmt.Sprintf("answers=%v", answers), func(t *testing.T) {
			h := newHarness(t)
			cfgDir := h.sup.cfg.SnapshotDirs[0].Path
			os.MkdirAll(cfgDir, 0o700)
			os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
			t.Setenv("SUPERVISOR_TEST_MARKER", filepath.Join(t.TempDir(), "crashed"))
			var first atomic.Int32
			h.sup.deps.Probe = func(_ context.Context, pid int, _ string) error {
				first.CompareAndSwap(0, int32(pid))
				os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
				if int32(pid) == first.Load() || answers {
					return nil
				}
				return errors.New("no answer on the local socket")
			}
			sum := dropInInbox(t, h.store, "once", Incoming{Explicit: true, RemoteVia: "lan"})
			t.Setenv(childBehaviorEnv(sum), "crash-once")
			h.start()
			h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
			if answers {
				h.eventually("it answers again and is approved", func() bool {
					st := h.state()
					return st.Approved != nil && st.Approved.SHA256 == sum
				})
				return
			}
			h.eventually("a restart that never answers is rejected", func() bool { return h.state().IsRejected(sum, "build-1") })
			h.eventually("the image agent runs again", func() bool { return h.status().Role == RoleImage })
			if got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json")); string(got) != "before" {
				t.Fatalf("the config must be restored, got %q", got)
			}
		})
	}
}

func TestRunKeepsTheConfigOfAnAgentApprovedBeforeAnOSUpdate(t *testing.T) {
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	os.MkdirAll(h.store.SnapshotDir(), 0o755)
	if err := snapshot(filepath.Join(h.store.SnapshotDir(), "confirmed"), h.sup.cfg.SnapshotDirs); err != nil {
		t.Fatal(err)
	}
	// The user's agent changed the config, was approved, then an OS update rebooted
	// into a build whose own agent is newer.
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("after"), 0o600)
	h.sup.cfg.ImageVersion = "2026.12.01-000000"
	if err := SaveState(h.store.StatePath(), State{Trial: &Trial{
		Agent: Agent{SHA256: "confirmed", Version: "2026.11.02-101010"}, RemoteVia: "lan",
		Confirmed: true, Build: "build-0", Boots: []string{"boot-0"},
	}}); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.eventually("the newer image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	if got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json")); string(got) != "after" {
		t.Fatalf("the config of an approved agent must stay, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.store.SnapshotDir(), "confirmed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("its snapshot must go")
	}
	if h.reloads.Load() != 0 {
		t.Fatal("nothing was restored, so NetworkManager need not reload")
	}
}

func TestRunReportsNoVerdictForAnAgentPromotedOntoABuildThatRejectedIt(t *testing.T) {
	h := newHarness(t)
	st := State{Trial: &Trial{Agent: Agent{SHA256: "kept", Version: "2026.11.02-101010"}, RemoteVia: "relay",
		Confirmed: true, Build: "build-0", Boots: []string{"boot-0"}}}
	st.Reject("kept", "2026.11.02-101010", "build-1", "agent is not listening on port 50051 or 50052")
	st.Reject("someone-else", "2026.11.03-000000", "build-1", "failed 3 times during its trial")
	os.MkdirAll(filepath.Dir(h.store.StatePath()), 0o755)
	if err := SaveState(h.store.StatePath(), st); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	// It was approved on the build it ran on; this build's old rejection is no new verdict.
	if s := h.status(); s.UpdateState != UpdateNone || !strings.Contains(s.Reason, "failed its checks on this OS build") {
		t.Fatalf("status = %+v", s)
	}
}

func TestRunKeepsTheApprovedAgentWhenTheTrialConfigCannotBeSaved(t *testing.T) {
	h := newHarness(t)
	_, prev := agentVariant(t, filepath.Dir(h.store.BinPath("x")), "previous")
	os.MkdirAll(filepath.Dir(h.store.StatePath()), 0o755)
	if err := SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: prev, Version: "2026.11.01-000000", Builds: []string{"build-1"}}}); err != nil {
		t.Fatal(err)
	}
	// A file where the snapshot directory belongs: the trial's config cannot be saved.
	if err := os.WriteFile(h.store.SnapshotDir(), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := dropInInbox(t, h.store, "new", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("the approved agent runs", func() bool { s := h.status(); return s.Role == RoleApproved && s.PID > 0 })
	if s := h.status(); s.SHA256 != prev || s.UpdateState != UpdateRejected || s.UpdateSHA256 != sum ||
		!strings.Contains(s.Reason, "could not save the agent config") {
		t.Fatalf("status = %+v", s)
	}
}

func TestRunLogsTheImageAgentsVersion(t *testing.T) {
	h := newHarness(t)
	core, logs := observer.New(zap.InfoLevel)
	h.sup.deps.Log = zap.New(core)
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	entries := logs.FilterMessage("starting agent").All()
	if len(entries) == 0 || entries[0].ContextMap()["version"] != h.sup.cfg.ImageVersion {
		t.Fatalf("starting agent logged %+v", entries)
	}
}

func TestRunKeepsThePreviousAgentUntilTheGraceEnds(t *testing.T) {
	h := newHarness(t)
	prevPath, prev := agentVariant(t, filepath.Dir(h.store.BinPath("x")), "previous")
	if err := SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: prev, Version: "2026.11.01-000000", Builds: []string{"build-1"}}}); err != nil {
		t.Fatal(err)
	}
	sum := dropInInbox(t, h.store, "new", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("the new agent is in its grace", func() bool { s := h.status(); return s.Grace && s.UpdateSHA256 == sum })
	if _, err := os.Stat(prevPath); err != nil {
		t.Fatalf("the fallback must stay through the grace: %v", err)
	}
	h.eventually("the grace ends", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
	h.eventually("the old agent's file is freed", func() bool { _, err := os.Stat(prevPath); return errors.Is(err, os.ErrNotExist) })
}

func TestRunRejectsAGraceRunThatKeepsExitingCleanly(t *testing.T) {
	h := newHarness(t)
	cfgDir := h.sup.cfg.SnapshotDirs[0].Path
	os.MkdirAll(cfgDir, 0o700)
	os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("before"), 0o600)
	t.Setenv("SUPERVISOR_TEST_MARKER", filepath.Join(t.TempDir(), "confirmed"))
	var first atomic.Int32
	h.sup.deps.Probe = func(_ context.Context, pid int, _ string) error {
		first.CompareAndSwap(0, int32(pid))
		os.WriteFile(filepath.Join(cfgDir, "provisioning.json"), []byte("trial wrote this"), 0o600)
		if int32(pid) == first.Load() {
			return nil
		}
		return errors.New("no answer on the local socket")
	}
	sum := dropInInbox(t, h.store, "clean", Incoming{Explicit: true, RemoteVia: "lan"})
	t.Setenv(childBehaviorEnv(sum), "confirm-then-clean")
	h.start()
	h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
	// Exiting 0 without answering is how a broken agent can look healthy to a
	// failure count that only sees crashes.
	h.eventually("its silent restarts reject it", func() bool { return h.state().IsRejected(sum, "build-1") })
	h.eventually("the image agent runs again", func() bool { return h.status().Role == RoleImage })
	if got, _ := os.ReadFile(filepath.Join(cfgDir, "provisioning.json")); string(got) != "before" {
		t.Fatalf("the config must be restored, got %q", got)
	}
}

func TestRunRejectsAnAgentThatStopsAnsweringInItsGrace(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.TrialDeadline = 5 * time.Second
	var calls atomic.Int32
	h.sup.deps.Probe = func(context.Context, int, string) error {
		// Answers long enough to be confirmed, then hangs.
		if calls.Add(1) <= 12 {
			return nil
		}
		return errors.New("the local socket does not answer")
	}
	sum := dropInInbox(t, h.store, "hangs", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
	h.eventually("the hang is rejected before the grace ends", func() bool { return h.state().IsRejected(sum, "build-1") })
	st := h.state()
	if st.Approved != nil && st.Approved.SHA256 == sum {
		t.Fatal("a hung agent must never be made final")
	}
	if r := st.Rejected[len(st.Rejected)-1]; !strings.Contains(r.Reason, "stopped answering") {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestRunRestartsAnApprovedAgentThatNeverAnswers(t *testing.T) {
	h := newHarness(t)
	_, prev := agentVariant(t, filepath.Dir(h.store.BinPath("x")), "silent")
	if err := SaveState(h.store.StatePath(), State{Approved: &Agent{SHA256: prev, Version: "2026.11.01-000000", Builds: []string{"build-1"}}}); err != nil {
		t.Fatal(err)
	}
	h.alive.Store(errors.New("no answer"))
	h.start()
	h.eventually("the approved agent runs", func() bool { s := h.status(); return s.Role == RoleApproved && s.PID > 0 })
	first := h.status().PID
	h.eventually("a hang from the start is restarted", func() bool { s := h.status(); return s.PID > 0 && s.PID != first })
}

func TestRunNeverRestartsForLivenessWithoutTheLocalSocket(t *testing.T) {
	h := newHarness(t)
	h.alive.Store(ErrNoLiveness)
	h.start()
	h.eventually("the image agent runs", func() bool { s := h.status(); return s.Role == RoleImage && s.PID > 0 })
	pid := h.status().PID
	time.Sleep(400 * time.Millisecond)
	if got := h.status().PID; got != pid {
		t.Fatalf("an agent without a local socket was restarted: %d -> %d", pid, got)
	}
}

func TestRunToleratesCleanRestartsThatAnsweredInTheGrace(t *testing.T) {
	h := newHarness(t)
	sum := dropInInbox(t, h.store, "restarts", Incoming{Explicit: true, RemoteVia: "lan"})
	t.Setenv(childBehaviorEnv(sum), "restarts-cleanly")
	h.start()
	h.eventually("the grace ends", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
}

func TestRunApprovesOnlyWhileTheLatestProbePasses(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.TrialDeadline = time.Second
	h.sup.cfg.GraceMissWindow = time.Minute
	var failing atomic.Bool
	h.sup.deps.Probe = func(context.Context, int, string) error {
		if failing.Load() {
			return errors.New("the local socket does not answer")
		}
		return nil
	}
	sum := dropInInbox(t, h.store, "flaky", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
	failing.Store(true)
	time.Sleep(h.sup.cfg.TrialDeadline + 300*time.Millisecond)
	if st := h.state(); st.Approved != nil && st.Approved.SHA256 == sum {
		t.Fatal("approved for good while its latest probe failed")
	}
	failing.Store(false)
	h.eventually("approved once it answers again", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
}

func TestRunRidesOutAShortStallInTheGrace(t *testing.T) {
	h := newHarness(t)
	h.sup.cfg.TrialDeadline = 1500 * time.Millisecond
	h.sup.cfg.GraceMissWindow = 600 * time.Millisecond
	var stallUntil atomic.Int64
	h.sup.deps.Probe = func(context.Context, int, string) error {
		if time.Now().UnixNano() < stallUntil.Load() {
			return errors.New("the local socket does not answer")
		}
		return nil
	}
	sum := dropInInbox(t, h.store, "busy", Incoming{Explicit: true, RemoteVia: "lan"})
	h.start()
	h.eventually("it is approved early", func() bool { return h.status().UpdateState == UpdateApproved })
	// Busy for less than the window, as during an OS install.
	stallUntil.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
	h.eventually("the grace ends", func() bool { st := h.state(); return st.Approved != nil && st.Approved.SHA256 == sum })
	if h.state().IsRejected(sum, "build-1") {
		t.Fatal("a short stall must not reject the agent")
	}
}

func TestRunStopsRestartingAnImageAgentThatNeverAnswers(t *testing.T) {
	h := newHarness(t)
	h.alive.Store(errors.New("connection refused"))
	h.start()
	pids := map[int]bool{}
	seen := func() int {
		if p := h.status().PID; p > 0 {
			pids[p] = true
		}
		return len(pids)
	}
	// The first run plus the capped restarts, however slow each start is.
	h.eventually("the capped restarts", func() bool { return seen() >= 1+imageLivenessRestarts })
	// Then it is left alone, for far longer than a restart takes.
	for quiet := time.Now().Add(3 * time.Second); time.Now().Before(quiet); time.Sleep(5 * time.Millisecond) {
		seen()
	}
	if len(pids) != 1+imageLivenessRestarts {
		t.Fatalf("saw %d image agent processes, want %d", len(pids), 1+imageLivenessRestarts)
	}
}

func TestUnitSweepSignalsAProcessStartedDuringIt(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "late.pid")
	// Starts another helper as it is asked to stop.
	sh := exec.Command("sh", "-c", `trap 'sleep 30 & echo $! > `+pidFile+`; exit 0' TERM; while :; do sleep 0.05; done`)
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	go sh.Wait()
	time.Sleep(200 * time.Millisecond)
	readPid := func() int {
		data, _ := os.ReadFile(pidFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return pid
	}
	t.Cleanup(func() {
		if pid := readPid(); pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	s := &Supervisor{cfg: Config{StopTimeout: 5 * time.Second}}
	s.deps.UnitProcs = func() []int {
		var procs []int
		for _, pid := range []int{sh.Process.Pid, readPid()} {
			if pid > 0 && running(pid) {
				procs = append(procs, pid)
			}
		}
		return procs
	}
	start := time.Now()
	s.sweepUnit()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the sweep waited %s for a helper it never asked to stop", took)
	}
}

func TestRunRemovesAnUploadTheAgentLeftHalfWritten(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(h.store.InboxDir(), 0o755)
	left := filepath.Join(h.store.InboxDir(), ".agent-update-123")
	os.WriteFile(left, make([]byte, 1024), 0o600)
	h.start()
	h.eventually("the abandoned upload is removed", func() bool { _, err := os.Stat(left); return errors.Is(err, os.ErrNotExist) })
}
