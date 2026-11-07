//go:build linux

package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type Config struct {
	Store        Store
	ImageAgent   string
	ImageVersion string
	Arch         string
	StatusPath   string
	// SnapshotDirs are restored when a trial fails (agent settings, NM connections).
	SnapshotDirs []SnapshotDir
	// Self is this binary's path, passed to the agent for `check`.
	Self string

	TrialDeadline time.Duration
	// MinTrialUptime keeps an agent that passes its checks and then crashes
	// from being approved.
	MinTrialUptime time.Duration
	ProbeEvery     time.Duration
	// GraceMissWindow is how long a trial in its grace may fail its probes, as
	// under the IO of an OS install, before it is rejected.
	GraceMissWindow time.Duration
	LivenessEvery   time.Duration
	LivenessMisses  int
	RestartDelay    time.Duration
	StopTimeout     time.Duration
}

type Deps struct {
	DataMounted func() bool
	Emergency   func() bool
	BootID      func() string
	Build       func() string
	// Uptime is the boot clock, which the trial deadline is measured on.
	Uptime     func() time.Duration
	RunVersion VersionRunner
	// OSGate commits or rolls back a pending OS update with the image agent.
	OSGate     func(context.Context) error
	Probe      func(ctx context.Context, pid int, sha string) error
	Alive      func(context.Context) error
	RemoteSeen func(pid int, via string) bool
	// Serving reports whether the agent listens on its network port, which it
	// opens only after its OS gate; it covers agents without a local socket.
	Serving func(pid int) bool
	// ReloadNetwork makes NetworkManager read restored profiles; it does not
	// watch its keyfiles.
	ReloadNetwork func() error
	// Wake says an automatic update is in the inbox.
	Wake <-chan struct{}
	// Updating reports whether the agent with pid is applying an update, which
	// a restart would cut off.
	Updating func(pid int) bool
	// UnitProcs lists the processes in the service's cgroup; nil skips the sweep.
	UnitProcs func() []int
	Log       *zap.Logger
}

const restorePause = "agent updates are off until the agent config is restored"

type Supervisor struct {
	cfg  Config
	deps Deps

	gateDone bool
	// gateUnconfirmed: this boot's OS gate failed, so the update may still be
	// reverted to the build the approved agent ran on.
	gateUnconfirmed bool
	// forceImage keeps the image agent running until it has run the OS
	// gate itself, when the supervisor could not.
	forceImage string
	paused     string
	// restorePending means a rejected trial's config could not be put back yet.
	restorePending bool
	// reapproved is the approved agent a user re-sent, which is its verdict.
	reapproved string
	// uploadSHA is the upload adoptInbox looked at last.
	uploadSHA     string
	lastUpdate    UpdateState
	lastUpdateSHA string
	lastReason    string
	// agentStarted is the boot-clock time the running agent started.
	agentStarted time.Duration
	// imageRestarts counts liveness restarts of the image agent.
	imageRestarts int
}

func New(cfg Config, deps Deps) *Supervisor {
	return &Supervisor{cfg: cfg, deps: deps}
}

// Run supervises the agent until ctx ends, then stops it.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.deps.OSGate(ctx); err != nil {
		s.deps.Log.Warn("OS update check did not run; the image agent will run it", zap.Error(err))
		s.forceImage = "the image agent is checking the OS update first"
		s.gateUnconfirmed = true
	} else {
		s.gateDone = true
	}
	for ctx.Err() == nil {
		d, trial := s.next(ctx)
		if ctx.Err() != nil {
			return nil
		}
		res := s.runChild(ctx, d, trial)
		if ctx.Err() != nil {
			return nil
		}
		s.afterRun(d, res)
		select {
		case <-ctx.Done():
		case <-time.After(s.cfg.RestartDelay):
		}
	}
	return nil
}

// next picks the agent and gets /data ready for it. Anything that goes wrong
// here falls back to the image agent.
func (s *Supervisor) next(ctx context.Context) (Decision, *Trial) {
	build, boot := s.deps.Build(), s.deps.BootID()
	in := Inputs{
		Emergency:     s.deps.Emergency(),
		ForceImage:    s.forceImage,
		DataMounted:   s.deps.DataMounted(),
		Build:         build,
		BootID:        boot,
		ImageVersion:  s.cfg.ImageVersion,
		Uptime:        s.deps.Uptime(),
		TrialDeadline: s.cfg.TrialDeadline,
		OSUnconfirmed: s.gateUnconfirmed,
	}
	commit := func() {}
	s.paused = ""
	usable := false
	switch {
	case in.Emergency:
		s.paused = "agent updates are off: the emergency flag is set"
	case in.ForceImage != "":
		s.paused = "agent updates are off until the OS update check finishes"
	case !in.DataMounted:
		s.paused = "agent updates are off: /data is not mounted"
	default:
		in.State, in.StateErr = s.loadState()
		if in.StateErr != nil {
			s.paused = fmt.Sprintf("agent updates are off: %v", in.StateErr)
			break
		}
		usable = true
		// Free a rejected binary's space first: a full disk is the usual reason
		// a restore fails. A new trial's snapshot must not capture a rejected
		// trial's config, so it waits for the restore.
		s.collectGarbage(in.State, "")
		if s.sweepSnapshots(in.State, build); s.restorePending {
			s.paused = restorePause
			break
		}
		removeAbandonedUploads(s.cfg.Store.InboxDir())
		var reason string
		reason, commit = s.adoptInbox(ctx, &in.State, build)
		if ctx.Err() != nil {
			return image("stopping"), nil
		}
		if reason != "" {
			s.setUpdate(UpdateRejected, s.uploadSHA, reason)
		}
	}

	d, st := Choose(in)
	trialEnded := in.State.Trial != nil && st.Trial == nil
	if t := in.State.Trial; d.TrialRejected != "" && !t.Revalidation {
		s.setUpdate(UpdateRejected, t.SHA256, d.TrialRejected)
	}
	if !usable {
		return d, nil
	}
	if err := SaveState(s.cfg.Store.StatePath(), st); err != nil {
		s.deps.Log.Error("cannot save the agent state", zap.Error(err))
		return image(fmt.Sprintf("cannot save the agent state: %v", err)), nil
	}
	commit()
	if t := in.State.Trial; t != nil && t.Confirmed && t.Build != build {
		// Approved on the build it passed on: the config it changed stays, even
		// when this build's own agent replaces it.
		os.RemoveAll(filepath.Join(s.cfg.Store.SnapshotDir(), t.SHA256))
	}
	if trialEnded {
		s.dropAutomaticUploads()
	}
	if s.reapproved != "" {
		s.setUpdate(UpdateApproved, s.reapproved, "")
		s.reapproved = ""
	}
	// Again with the state Choose returned: a trial it just rejected is no
	// longer kept, and its space may be what the restore below needs.
	s.collectGarbage(st, "")
	// Before the next agent starts, so it never runs on a config a rejected
	// trial changed; a restore that fails is retried while that agent runs.
	s.sweepSnapshots(st, build)
	switch {
	case s.restorePending && s.paused == "":
		s.paused = restorePause
	case !s.restorePending && s.paused == restorePause:
		s.paused = ""
	}
	if d.Role == RoleImage {
		return d, nil
	}

	if sum, err := fileSHA256(s.cfg.Store.BinPath(d.SHA256)); err != nil || sum != d.SHA256 {
		const damaged = "the agent file is missing or changed on disk"
		if st.Trial != nil && st.Trial.SHA256 == d.SHA256 {
			st.Reject(d.SHA256, d.Version, build, damaged)
			st.Trial = nil
		}
		if st.Approved != nil && st.Approved.SHA256 == d.SHA256 {
			// The file failed, not the agent: the daily update may install it again.
			st.Approved = nil
		}
		s.save(st)
		return image(damaged), nil
	}

	if d.Role != RoleTrial {
		return d, nil
	}
	t := st.Trial
	if !t.Revalidation && !t.Confirmed {
		snap := filepath.Join(s.cfg.Store.SnapshotDir(), t.SHA256)
		// Any error, not only a missing one: a trial never runs without its snapshot.
		if _, err := os.Stat(snap); err != nil {
			if err := snapshot(snap, s.cfg.SnapshotDirs); err != nil {
				reason := fmt.Sprintf("could not save the agent config before the trial: %v", err)
				st.Reject(t.SHA256, t.Version, build, reason)
				st.Trial = nil
				s.setUpdate(UpdateRejected, t.SHA256, reason)
				if !s.save(st) {
					return image(reason), nil
				}
				// Choose again: the approved agent did nothing wrong.
				return s.next(ctx)
			}
		}
		s.setUpdate(UpdateTrial, t.SHA256, "")
	}
	return d, t
}

// loadState sets a corrupt state.json aside: an empty state only means the
// image agent runs, while keeping the corrupt one would block updates forever.
func (s *Supervisor) loadState() (State, error) {
	if err := s.prepareStore(); err != nil {
		return State{}, err
	}
	path := s.cfg.Store.StatePath()
	st, err := LoadState(path)
	if errors.Is(err, ErrCorruptState) {
		s.deps.Log.Error("agent state is corrupt; starting from an empty one", zap.Error(err))
		if err := os.Rename(path, path+".corrupt"); err != nil {
			return State{}, err
		}
		return State{}, nil
	}
	return st, err
}

type runResult struct {
	exit        Exit
	startErr    error
	trialFailed string
	// unconfirmed: the trial passed its local checks and only missed the remote confirm.
	unconfirmed  bool
	livenessDead bool
	// silentExit: a grace run exited 0 before it answered, which a crash
	// count alone would retry forever.
	silentExit bool
}

func (s *Supervisor) runChild(ctx context.Context, d Decision, t *Trial) runResult {
	path := s.cfg.ImageAgent
	if d.Role != RoleImage {
		path = s.cfg.Store.BinPath(d.SHA256)
	}
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(),
		EnvSupervisor+"="+s.cfg.Self,
		EnvStore+"="+s.cfg.Store.Dir)
	if s.gateDone {
		cmd.Env = append(cmd.Env, "WENDY_OS_GATE=done")
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Its own group, so helpers it leaves behind can be cleaned up the way
	// systemd did when it restarted the agent itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	version := d.Version
	if d.Role == RoleImage {
		version = s.cfg.ImageVersion
	}
	s.deps.Log.Info("starting agent", zap.String("role", string(d.Role)), zap.String("version", version), zap.String("reason", d.Reason))
	started := time.Now()
	s.agentStarted = s.deps.Uptime()
	if err := cmd.Start(); err != nil {
		s.writeStatus(d, 0)
		return runResult{startErr: err}
	}
	pid := cmd.Process.Pid
	defer s.reapGroup(pid)
	s.writeStatus(d, pid)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// A trial is judged until it passes its checks, then runs out its deadline
	// as a grace, with its fallback still kept.
	inTrial := t != nil
	judging := inTrial && !t.Confirmed
	// In its grace every run must answer the local probe again, or a restart
	// into a hung agent would be approved; it is then probed until approval.
	answeredInGrace, graceOver, graceOK := false, false, false
	var lastAnswer time.Time
	var deadline, runDeadline <-chan time.Time
	if inTrial {
		// One deadline for all of the trial's runs in this boot.
		timer := time.NewTimer(t.StartedAt + s.cfg.TrialDeadline - s.deps.Uptime())
		defer timer.Stop()
		deadline = timer.C
		if t.Confirmed {
			rt := time.NewTimer(s.cfg.TrialDeadline)
			defer rt.Stop()
			runDeadline = rt.C
		}
	}
	probe := time.NewTicker(s.cfg.ProbeEvery)
	defer probe.Stop()
	live := time.NewTicker(s.cfg.LivenessEvery)
	defer live.Stop()
	var lastProbeErr error
	misses, answered := 0, false

	for {
		select {
		case err := <-exited:
			res := runResult{exit: exitOf(err, time.Since(started))}
			res.silentExit = inTrial && !judging && !answeredInGrace && !CountsAsFailure(res.exit)
			return res
		case <-ctx.Done():
			s.stop(cmd, exited)
			return runResult{exit: Exit{Stopped: true}}
		case <-deadline:
			if !judging {
				graceOver, deadline = true, nil
				if graceOK && s.endGrace(t, &d, pid) {
					inTrial = false
				}
				continue
			}
			s.stop(cmd, exited)
			reason := fmt.Sprintf("did not pass its checks within %s", s.cfg.TrialDeadline)
			if lastProbeErr != nil {
				reason += ": " + lastProbeErr.Error()
			}
			return runResult{exit: Exit{Stopped: true}, trialFailed: reason, unconfirmed: errors.As(lastProbeErr, new(notReached))}
		case <-runDeadline:
			runDeadline = nil
			if inTrial && !answeredInGrace {
				s.stop(cmd, exited)
				reason := "did not answer again after it restarted"
				if lastProbeErr != nil {
					reason += ": " + lastProbeErr.Error()
				}
				return runResult{exit: Exit{Stopped: true}, trialFailed: reason}
			}
		case <-probe.C:
			if s.restorePending && !s.updating(pid) && s.retryRestore(d.SHA256) {
				// Back onto the restored config.
				s.stop(cmd, exited)
				return runResult{exit: Exit{Stopped: true}}
			}
			if inTrial && !judging {
				lastProbeErr = s.deps.Probe(ctx, pid, t.SHA256)
				graceOK = lastProbeErr == nil
				switch {
				case graceOK:
					answeredInGrace, answered, runDeadline, lastAnswer = true, true, nil, time.Now()
				case answeredInGrace && time.Since(lastAnswer) >= s.cfg.GraceMissWindow:
					s.stop(cmd, exited)
					return runResult{exit: Exit{Stopped: true}, trialFailed: "stopped answering during its grace: " + lastProbeErr.Error()}
				}
			}
			if inTrial && graceOver && graceOK && s.endGrace(t, &d, pid) {
				inTrial = false
			}
			if !judging {
				continue
			}
			if lastProbeErr = s.judge(ctx, pid, t, time.Since(started)); lastProbeErr == nil {
				if err := s.confirm(t); err != nil {
					lastProbeErr = err
					continue
				}
				judging, d.Grace = false, true
				answeredInGrace, answered, graceOK, lastAnswer = true, true, true, time.Now()
				s.writeStatus(d, pid)
			}
		case <-s.deps.Wake:
			// A trial is judged first; its outcome drops what was fetched before it.
			// The next fetch wakes it again after an OS update that did not reboot.
			if inTrial || s.paused != "" || !s.inboxHasUpload() || s.uploadRunning() || s.updating(pid) {
				continue
			}
			s.deps.Log.Info("an automatic agent update is waiting; restarting to check it")
			s.stop(cmd, exited)
			return runResult{exit: Exit{Stopped: true}}
		case <-live.C:
			if s.deps.RemoteSeen(pid, ViaCloud) {
				// The fetch may run only near boot, before the relay is up.
				os.Remove(s.cfg.Store.CloudDeferralsPath())
			}
			if inTrial {
				// The probe above watches a trial, grace included.
				continue
			}
			err := s.deps.Alive(ctx)
			if d.Role == RoleImage && s.forceImage != "" && !s.updating(pid) && (err == nil || s.deps.Serving(pid)) {
				// It answers and listens only after its own OS gate. Restart, so
				// next() picks again with everything recomputed.
				s.forceImage, s.gateDone = "", true
				s.stop(cmd, exited)
				return runResult{exit: Exit{Stopped: true}}
			}
			// Without the local socket there is nothing to ping. The image agent
			// is spared until its OS gate opens the socket.
			if err == nil {
				misses, answered = 0, true
			} else if !errors.Is(err, ErrNoLiveness) && (answered || s.forceImage == "") && !s.imageRestartsSpent(d) {
				misses++
				s.deps.Log.Warn("agent did not answer", zap.Int("misses", misses), zap.Error(err))
				if misses >= s.cfg.LivenessMisses {
					if d.Role == RoleImage {
						s.imageRestarts++
					}
					s.stop(cmd, exited)
					return runResult{exit: Exit{Stopped: true}, livenessDead: true}
				}
			}
		}
	}
}

func (s *Supervisor) judge(ctx context.Context, pid int, t *Trial, up time.Duration) error {
	if err := s.deps.Probe(ctx, pid, t.SHA256); err != nil {
		return err
	}
	if t.RemoteVia != "" && !s.deps.RemoteSeen(pid, t.RemoteVia) {
		if t.RemoteVia == ViaCloud {
			return notReached("the new agent has not reached the cloud yet")
		}
		return notReached("the client that sent the update has not reached the new agent yet")
	}
	if up < s.cfg.MinTrialUptime {
		return fmt.Errorf("the new agent has run for %s, less than %s", up.Round(time.Second), s.cfg.MinTrialUptime)
	}
	return nil
}

// notReached is a judgement that lacks only the remote confirm, which a
// network blip can cause; such a rejection is retried.
type notReached string

func (e notReached) Error() string { return string(e) }

// confirm records that the trial passed its checks and reports it approved;
// it stays the trial until its grace ends.
func (s *Supervisor) confirm(t *Trial) error {
	st, err := LoadState(s.cfg.Store.StatePath())
	if err != nil {
		return err
	}
	if st.Trial == nil || st.Trial.SHA256 != t.SHA256 {
		return errors.New("the trial changed on disk")
	}
	st.Trial.Confirmed, st.Trial.Build = true, s.deps.Build()
	if err := SaveState(s.cfg.Store.StatePath(), st); err != nil {
		return err
	}
	t.Confirmed, t.Build = true, st.Trial.Build
	s.deps.Log.Info("new agent passed its checks", zap.String("version", t.Version), zap.Bool("revalidation", t.Revalidation))
	if !t.Revalidation {
		s.setUpdate(UpdateApproved, t.SHA256, "")
	}
	return nil
}

// endGrace approves the trial for good; it reports whether that worked, and
// is retried on the next probe if it did not.
func (s *Supervisor) endGrace(t *Trial, d *Decision, pid int) bool {
	if err := s.promote(t, *d); err != nil {
		s.deps.Log.Warn("cannot approve the agent yet", zap.Error(err))
		return false
	}
	d.Role, d.Revalidation, d.Automatic, d.Grace = RoleApproved, false, false, false
	s.writeStatus(*d, pid)
	return true
}

func (s *Supervisor) promote(t *Trial, d Decision) error {
	st, err := LoadState(s.cfg.Store.StatePath())
	if err != nil {
		return err
	}
	if st.Trial == nil || st.Trial.SHA256 != t.SHA256 {
		return errors.New("the trial changed on disk")
	}
	st.Promote(s.deps.Build())
	if err := SaveState(s.cfg.Store.StatePath(), st); err != nil {
		return err
	}
	a := st.Approved
	s.deps.Log.Info("agent approved", zap.String("version", a.Version), zap.Bool("revalidation", t.Revalidation))
	if !t.Revalidation {
		s.setUpdate(UpdateApproved, a.SHA256, "")
	}
	os.RemoveAll(filepath.Join(s.cfg.Store.SnapshotDir(), t.SHA256))
	s.dropAutomaticUploads()
	s.collectGarbage(st, d.SHA256)
	return nil
}

func (s *Supervisor) afterRun(d Decision, res runResult) {
	if d.Role == RoleImage {
		return
	}
	st, err := LoadState(s.cfg.Store.StatePath())
	if err != nil {
		return
	}
	switch {
	case res.trialFailed != "" && st.Trial != nil && st.Trial.SHA256 == d.SHA256:
		// The config snapshot is restored by the sweep before the next agent starts.
		if res.unconfirmed {
			st.RejectUnconfirmed(d.SHA256, d.Version, s.deps.Build(), res.trialFailed)
		} else {
			st.Reject(d.SHA256, d.Version, s.deps.Build(), res.trialFailed)
		}
		revalidation := st.Trial.Revalidation
		st.Trial = nil
		s.save(st)
		// Fetched before this trial; its verdict must be published first.
		s.dropAutomaticUploads()
		s.deps.Log.Warn("new agent rejected", zap.String("version", d.Version), zap.String("reason", res.trialFailed))
		if !revalidation {
			s.setUpdate(UpdateRejected, d.SHA256, res.trialFailed)
		}
	case res.startErr != nil, res.livenessDead, res.silentExit, CountsAsFailure(res.exit):
		s.deps.Log.Warn("agent failed", zap.String("version", d.Version), zap.Error(res.startErr), zap.Int("code", res.exit.Code))
		st.RecordFailure(d.SHA256, s.deps.BootID())
		s.save(st)
	}
}

// sweepSnapshots restores the config from any snapshot whose trial ended
// without approval and drops the rest. A snapshot is removed only once its
// restore worked, so a crash or a full disk mid-restore retries it.
func (s *Supervisor) sweepSnapshots(st State, build string) {
	s.restorePending = false
	restored := false
	defer func() {
		if restored {
			if err := s.deps.ReloadNetwork(); err != nil {
				s.deps.Log.Warn("could not reload NetworkManager after the restore", zap.Error(err))
			}
		}
	}()
	entries, _ := os.ReadDir(s.cfg.Store.SnapshotDir())
	for _, e := range entries {
		name, path := e.Name(), filepath.Join(s.cfg.Store.SnapshotDir(), e.Name())
		if strings.HasSuffix(name, ".partial") {
			os.RemoveAll(path)
			continue
		}
		if t := st.Trial; t != nil && !t.Revalidation && t.SHA256 == name {
			continue
		}
		approved := st.Approved != nil && st.Approved.SHA256 == name && !st.IsRejected(name, build)
		if !approved {
			if err := restore(path, s.cfg.SnapshotDirs); err != nil {
				s.deps.Log.Error("could not restore the agent config; will retry", zap.String("sha256", name), zap.Error(err))
				s.restorePending = true
				continue
			}
			s.deps.Log.Info("restored the agent config from before a rejected trial", zap.String("sha256", name))
			restored = true
		}
		os.RemoveAll(path)
	}
}

// retryRestore reports whether a pending restore has now gone through.
func (s *Supervisor) retryRestore(running string) bool {
	st, err := LoadState(s.cfg.Store.StatePath())
	if err != nil {
		return false
	}
	s.collectGarbage(st, running)
	s.sweepSnapshots(st, s.deps.Build())
	return !s.restorePending
}

func (s *Supervisor) stop(cmd *exec.Cmd, exited <-chan error) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(s.cfg.StopTimeout):
		cmd.Process.Kill()
		<-exited
	}
}

// reapGroup ends whatever the agent left running in its process group.
func (s *Supervisor) reapGroup(pgid int) {
	defer s.sweepUnit()
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return
	}
	for deadline := time.Now().Add(s.cfg.StopTimeout); time.Now().Before(deadline); {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

// imageLivenessRestarts caps restarts of the image agent for not answering:
// nothing falls back from it, and one whose local socket failed would
// otherwise be restarted all day.
const imageLivenessRestarts = 2

func (s *Supervisor) imageRestartsSpent(d Decision) bool {
	return d.Role == RoleImage && s.imageRestarts >= imageLivenessRestarts
}

// sweepUnit stops what the agent left outside its group, such as a helper in
// its own session, as systemd did when it restarted the agent itself. Nothing
// else of the supervisor's runs between agents.
func (s *Supervisor) sweepUnit() {
	if s.deps.UnitProcs == nil {
		return
	}
	others := func() []int {
		return slices.DeleteFunc(s.deps.UnitProcs(), func(pid int) bool { return pid == os.Getpid() })
	}
	// A helper can start another as it dies, so each pass signals newcomers too.
	signalled := map[int]bool{}
	for deadline := time.Now().Add(s.cfg.StopTimeout); ; {
		left := others()
		if len(left) == 0 {
			return
		}
		late := time.Now().After(deadline)
		for _, pid := range left {
			switch {
			case late:
				syscall.Kill(pid, syscall.SIGKILL)
			case !signalled[pid]:
				syscall.Kill(pid, syscall.SIGTERM)
				signalled[pid] = true
			}
		}
		if late {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// collectGarbage removes binaries nothing points at; running keeps the one in
// use, and an upload still named in the inbox is kept for its next pass.
func (s *Supervisor) collectGarbage(st State, running string) {
	keep := map[string]bool{running: true}
	metas, _ := filepath.Glob(filepath.Join(s.cfg.Store.InboxDir(), "*.json"))
	for _, m := range metas {
		keep[strings.TrimSuffix(filepath.Base(m), ".json")] = true
	}
	if st.Approved != nil {
		keep[st.Approved.SHA256] = true
	}
	if st.Trial != nil {
		keep[st.Trial.SHA256] = true
	}
	entries, _ := os.ReadDir(filepath.Dir(s.cfg.Store.BinPath("x")))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.Remove(s.cfg.Store.BinPath(e.Name()))
		}
	}
}

func (s *Supervisor) inboxHasUpload() bool {
	metas, _ := filepath.Glob(filepath.Join(s.cfg.Store.InboxDir(), "*.json"))
	return len(metas) > 0
}

func (s *Supervisor) updating(pid int) bool {
	return s.deps.Updating != nil && s.deps.Updating(pid)
}

// uploadStale is when an agent upload's temp file is taken as abandoned.
const uploadStale = 10 * time.Minute

// uploadRunning reports whether the agent is receiving an upload, which a
// restart would cut off; that upload restarts the agent itself anyway.
func (s *Supervisor) uploadRunning() bool {
	temps, _ := filepath.Glob(filepath.Join(s.cfg.Store.InboxDir(), ".agent-update-*"))
	for _, p := range temps {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < uploadStale {
			return true
		}
	}
	return false
}

// dropAutomaticUploads removes the daily update's uploads, which were fetched
// for a state a trial has just changed; the next fetch decides again.
func (s *Supervisor) dropAutomaticUploads() {
	metas, _ := filepath.Glob(filepath.Join(s.cfg.Store.InboxDir(), "*.json"))
	for _, m := range metas {
		var in Incoming
		data, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(data, &in) != nil || in.Replaces == "" {
			continue
		}
		os.Remove(m)
		os.Remove(strings.TrimSuffix(m, ".json"))
	}
}

func (s *Supervisor) prepareStore() error {
	for _, dir := range []string{s.cfg.Store.Dir, filepath.Dir(s.cfg.Store.BinPath("x")), s.cfg.Store.InboxDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) save(st State) bool {
	if err := SaveState(s.cfg.Store.StatePath(), st); err != nil {
		s.deps.Log.Error("cannot save the agent state", zap.Error(err))
		return false
	}
	return true
}

func (s *Supervisor) setUpdate(state UpdateState, sha, reason string) {
	s.lastUpdate, s.lastUpdateSHA, s.lastReason = state, sha, reason
}

func (s *Supervisor) writeStatus(d Decision, pid int) {
	reason := s.lastReason
	if reason == "" && d.Role == RoleImage {
		reason = d.Reason
	}
	role := d.Role
	if d.Grace {
		// Clients treat it as approved: uploads and OS updates go ahead.
		role = RoleApproved
	}
	err := writeStatus(s.cfg.StatusPath, Status{
		Role:          role,
		Version:       d.Version,
		SHA256:        d.SHA256,
		PID:           pid,
		ImageVersion:  s.cfg.ImageVersion,
		UpdateState:   s.lastUpdate,
		UpdateSHA256:  s.lastUpdateSHA,
		Reason:        reason,
		Revalidation:  d.Role == RoleTrial && d.Revalidation && !d.Grace,
		UpdatesPaused: s.paused,
		Automatic:     d.Role == RoleTrial && d.Automatic && !d.Grace,
		Grace:         d.Grace,
		AgentStarted:  s.agentStarted,
	})
	if err != nil {
		s.deps.Log.Warn("cannot write the supervisor status", zap.Error(err))
	}
}

func exitOf(err error, ran time.Duration) Exit {
	e := Exit{Ran: ran}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			e.Signaled = true
		} else {
			e.Code = ee.ExitCode()
		}
	}
	return e
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
