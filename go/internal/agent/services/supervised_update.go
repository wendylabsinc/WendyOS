package services

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/supervisor"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// updateTempTarget is the path the update temp file is created next to.
func updateTempTarget(execPath string) string {
	if inbox, ok := supervisor.InboxFromEnv(); ok {
		return filepath.Join(inbox, "wendy-agent")
	}
	return execPath
}

// supervisorStatusPath is a variable so tests can point it elsewhere.
var supervisorStatusPath = supervisor.StatusPath

// supervisorMarkDir is where the agent leaves marks for its supervisor.
var supervisorMarkDir = supervisor.RemoteSeenDir

var wakeHolds struct {
	sync.Mutex
	n int
}

// holdSupervisorWake keeps the supervisor from restarting this agent while it
// applies an update, which the restart would cut off. Holds nest: the mark
// goes when the last one is released.
func holdSupervisorWake() func() {
	if _, ok := supervisor.InboxFromEnv(); !ok {
		return func() {}
	}
	path := filepath.Join(supervisorMarkDir, supervisor.UpdateMark)
	wakeHolds.Lock()
	defer wakeHolds.Unlock()
	if wakeHolds.n == 0 {
		if err := os.MkdirAll(supervisorMarkDir, 0o755); err != nil {
			return func() {}
		}
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return func() {}
		}
	}
	wakeHolds.n++
	var once sync.Once
	return func() {
		once.Do(func() {
			wakeHolds.Lock()
			defer wakeHolds.Unlock()
			if wakeHolds.n--; wakeHolds.n == 0 {
				os.Remove(path)
			}
		})
	}
}

// onlineUpdateVia is the remote confirm for an online update's trial. The device
// downloads it itself, like the daily update, so the app that asked need not
// come back; the cloud is required only while this agent is connected to it.
func onlineUpdateVia() string {
	if supervisor.RemoteSeenFunc(supervisorMarkDir)(os.Getpid(), supervisor.ViaCloud) {
		return supervisor.ViaCloud
	}
	return ""
}

// ownTrial reads the supervisor's status rather than the environment, because
// this same process stays running once its trial is approved.
func ownTrial() (supervisor.Status, bool) {
	if _, ok := supervisor.InboxFromEnv(); !ok {
		return supervisor.Status{}, false
	}
	st, err := supervisor.ReadStatus(supervisorStatusPath)
	return st, err == nil && st.Role == supervisor.RoleTrial && !st.Revalidation && st.PID == os.Getpid()
}

func trialInProgress() bool {
	_, ok := ownTrial()
	return ok
}

const automaticTrialMessage = "an automatic agent update is being verified; retry the OS update in a few minutes"

// automaticTrialInProgress: no client waits on this trial, so an OS update
// should not wait on it either.
func automaticTrialInProgress() bool {
	st, ok := ownTrial()
	return ok && st.Automatic
}

// stagedSHA is the upload this agent handed to the supervisor. Until the agent
// exits for it, the supervisor's status still describes the previous update.
var stagedSHA atomic.Value

// refuseUpdates turns an upload away when the supervisor could not take it,
// or when it would replace an agent that is still being judged.
func refuseUpdates() error {
	if _, ok := supervisor.InboxFromEnv(); !ok {
		return nil
	}
	if reason := supervisor.PausedReason(supervisorStatusPath); reason != "" {
		return status.Error(codes.FailedPrecondition, reason)
	}
	if trialInProgress() {
		return status.Error(codes.FailedPrecondition, "an agent update is being verified; retry in a few minutes")
	}
	return nil
}

// runSupervisorCheck runs the supervisor's pre-switch checks on a staged
// upload, so a bad agent is refused while the client is still connected.
var runSupervisorCheck = func(ctx context.Context, path, sha string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, os.Getenv(supervisor.EnvSupervisor), "check", "--sha256", sha, path).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// stageForSupervisor hands a verified upload to the supervisor, which runs it
// as a trial once this agent exits.
func stageForSupervisor(ctx context.Context, tmpFile *os.File, tmpPath, inbox, sha, expected, via string) error {
	if expected == "" {
		return status.Error(codes.InvalidArgument, "the agent supervisor needs the sha256 of the new agent; update the CLI")
	}
	if err := tmpFile.Sync(); err != nil {
		return status.Errorf(codes.Internal, "failed to sync the update: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		return status.Errorf(codes.Internal, "failed to close the update: %v", err)
	}
	staged := filepath.Join(inbox, sha)
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return status.Errorf(codes.Internal, "failed to prepare the update: %v", err)
	}
	if err := os.Rename(tmpPath, staged); err != nil {
		return status.Errorf(codes.Internal, "failed to stage the update: %v", err)
	}
	if err := runSupervisorCheck(ctx, staged, sha); err != nil {
		os.Remove(staged)
		return status.Errorf(codes.FailedPrecondition, "new agent failed its checks: %v", err)
	}
	if err := supervisor.WriteIncoming(inbox, supervisor.Incoming{SHA256: sha, RemoteVia: via, Explicit: true}); err != nil {
		os.Remove(staged)
		return status.Errorf(codes.Internal, "failed to stage the update: %v", err)
	}
	stagedSHA.Store(sha)
	return nil
}

var trialPollEvery = 2 * time.Second

// osUpdateTrialGate holds an OS update while this agent is on trial: it is
// refused during an automatic trial and otherwise waits for the verdict. It
// returns the refusal, or "".
func osUpdateTrialGate(ctx context.Context, waiting func()) string {
	if automaticTrialInProgress() {
		return automaticTrialMessage
	}
	if err := waitForTrialVerdict(ctx, 4*time.Minute, waiting); err != nil {
		return err.Error()
	}
	return ""
}

// waitForTrialVerdict holds an OS update until this agent's trial is decided,
// so an old CLI's `os update` (agent step, then OS step at once) still works.
func waitForTrialVerdict(ctx context.Context, limit time.Duration, waiting func()) error {
	deadline := time.Now().Add(limit)
	for trialInProgress() {
		if time.Now().After(deadline) {
			return errors.New("the agent update is still being verified; retry in a few minutes")
		}
		waiting()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(trialPollEvery):
		}
	}
	return nil
}

// supervisionStatus reports the supervisor's view, or nil without a supervisor.
func supervisionStatus() *agentpb.AgentSupervision {
	if _, ok := supervisor.InboxFromEnv(); !ok {
		return nil
	}
	st, err := supervisor.ReadStatus(supervisorStatusPath)
	if err != nil {
		return nil
	}
	sup := &agentpb.AgentSupervision{
		Role:         string(st.Role),
		ImageVersion: st.ImageVersion,
		UpdateState:  string(st.UpdateState),
		Reason:       st.Reason,
		UpdateSha256: st.UpdateSHA256,
	}
	if sha, _ := stagedSHA.Load().(string); sha != "" {
		sup.UpdateState, sup.UpdateSha256, sup.Reason = string(supervisor.UpdateTrial), sha, ""
	}
	return sup
}
