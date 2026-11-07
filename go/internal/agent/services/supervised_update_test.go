package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/supervisor"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type supervisedEnv struct {
	inbox   string
	checked []string
	// held records whether this agent held the update mark at each check.
	held []bool
}

// superviseForTest makes this test agent look started by the supervisor.
func superviseForTest(t *testing.T, checkErr error) *supervisedEnv {
	t.Helper()
	store := t.TempDir()
	env := &supervisedEnv{inbox: filepath.Join(store, "inbox")}
	if err := os.MkdirAll(env.inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WENDY_AGENT_SUPERVISOR", "/opt/wendyos/bin/wendy-agent-supervisor")
	t.Setenv("WENDY_AGENT_STORE", store)
	stagedSHA.Store("")
	t.Cleanup(func() { stagedSHA.Store("") })
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleApproved, PID: os.Getpid()})
	orig := runSupervisorCheck
	marks := t.TempDir()
	origMarks := supervisorMarkDir
	supervisorMarkDir = marks
	t.Cleanup(func() { supervisorMarkDir = origMarks })
	runSupervisorCheck = func(_ context.Context, path, sha string) error {
		env.checked = append(env.checked, path)
		env.held = append(env.held, supervisor.UpdatingFunc(marks)(os.Getpid()))
		return checkErr
	}
	t.Cleanup(func() { runSupervisorCheck = orig })
	return env
}

func writeSupervisorStatus(t *testing.T, st supervisor.Status) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "status.json")
	data, _ := json.Marshal(st)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := supervisorStatusPath
	supervisorStatusPath = path
	t.Cleanup(func() { supervisorStatusPath = orig })
}

func uploadFixture() ([]byte, string) {
	data := []byte("new-agent-binary-for-the-supervisor")
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

func TestSupervisedUpdateStagesInTheInboxAndLeavesTheRunningBinary(t *testing.T) {
	env := superviseForTest(t, nil)
	client, execPath, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	data, sha := uploadFixture()

	stream, err := client.UpdateAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sendV1UpdateStream(t, stream, data, sha, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got, _ := os.ReadFile(execPath); string(got) != "original-binary-contents" {
		t.Fatalf("the running binary must not change, got %q", got)
	}
	staged := filepath.Join(env.inbox, sha)
	if got, err := os.ReadFile(staged); err != nil || string(got) != string(data) {
		t.Fatalf("the upload must be in the inbox: %v", err)
	}
	if fi, _ := os.Stat(staged); fi.Mode().Perm() != 0o755 {
		t.Fatalf("staged mode = %v", fi.Mode().Perm())
	}
	var meta supervisor.Incoming
	raw, err := os.ReadFile(filepath.Join(env.inbox, sha+".json"))
	if err != nil {
		t.Fatalf("the metadata marks the upload complete: %v", err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil || meta.SHA256 != sha || !meta.Explicit {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
	if len(env.checked) != 1 || env.checked[0] != staged {
		t.Fatalf("the supervisor must check the staged file, checked %v", env.checked)
	}
	if !env.held[0] {
		t.Fatal("a wake during the upload would cut it off: the update mark must be held")
	}
}

func TestSupervisedUpdateReportsAFailedCheckAtOnce(t *testing.T) {
	env := superviseForTest(t, errors.New("built for a different CPU architecture"))
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	data, sha := uploadFixture()

	stream, _ := client.UpdateAgent(context.Background())
	err := sendV1UpdateStream(t, stream, data, sha, nil)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	// The CLI matches this prefix to go on with the agent the device kept.
	if msg := status.Convert(err).Message(); !strings.HasPrefix(msg, "new agent failed its checks: ") {
		t.Fatalf("the CLI no longer recognises this refusal: %q", msg)
	}
	if entries, _ := os.ReadDir(env.inbox); len(entries) != 0 {
		t.Fatalf("a rejected upload must not stay in the inbox, found %d entries", len(entries))
	}
}

func TestSupervisedUpdateNeedsTheSHA256(t *testing.T) {
	env := superviseForTest(t, nil)
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	data, _ := uploadFixture()

	stream, _ := client.UpdateAgent(context.Background())
	if err := sendV1UpdateStream(t, stream, data, "", nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if entries, _ := os.ReadDir(env.inbox); len(entries) != 0 {
		t.Fatal("nothing may be staged without a sha256")
	}
}

func TestSupervisedUpdateIsRefusedDuringATrial(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid()})
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	data, sha := uploadFixture()

	stream, _ := client.UpdateAgent(context.Background())
	if err := sendV1UpdateStream(t, stream, data, sha, nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition while a trial runs, got %v", err)
	}
}

func TestSupervisedUpdateV2StagesInTheInbox(t *testing.T) {
	env := superviseForTest(t, nil)
	client, execPath, cleanup := startAgentUpdateServer(t, nil)
	defer cleanup()
	data, sha := uploadFixture()

	stream, err := client.UpdateAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sendUpdateStream(t, stream, data, sha, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ := os.ReadFile(execPath); string(got) == string(data) {
		t.Fatal("the running binary must not change")
	}
	if _, err := os.Stat(filepath.Join(env.inbox, sha+".json")); err != nil {
		t.Fatalf("the upload must be staged: %v", err)
	}
	if len(env.held) != 1 || !env.held[0] {
		t.Fatalf("the update mark must be held while the upload is checked, got %v", env.held)
	}
}

func TestTrialInProgressFollowsTheSupervisorStatus(t *testing.T) {
	superviseForTest(t, nil)
	cases := []struct {
		name string
		st   supervisor.Status
		want bool
	}{
		{"this agent on trial", supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid()}, true},
		{"this agent approved since it started", supervisor.Status{Role: supervisor.RoleApproved, PID: os.Getpid()}, false},
		{"a stale status of another process", supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid() + 1}, false},
		{"this agent being re-checked after an OS update", supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid(), Revalidation: true}, false},
	}
	for _, c := range cases {
		writeSupervisorStatus(t, c.st)
		if got := trialInProgress(); got != c.want {
			t.Errorf("%s: trialInProgress = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWaitForTrialVerdictReturnsOnceApproved(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid()})
	orig := trialPollEvery
	trialPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { trialPollEvery = orig })

	go func() {
		time.Sleep(50 * time.Millisecond)
		data, _ := json.Marshal(supervisor.Status{Role: supervisor.RoleApproved, PID: os.Getpid()})
		os.WriteFile(supervisorStatusPath, data, 0o644)
	}()
	waits := 0
	if err := waitForTrialVerdict(context.Background(), time.Second, func() { waits++ }); err != nil {
		t.Fatalf("waitForTrialVerdict: %v", err)
	}
	if waits == 0 {
		t.Fatal("the client must see that the agent update is being verified")
	}
}

func TestWaitForTrialVerdictGivesUp(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid()})
	orig := trialPollEvery
	trialPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { trialPollEvery = orig })
	if err := waitForTrialVerdict(context.Background(), 50*time.Millisecond, func() {}); err == nil {
		t.Fatal("want an error when the trial does not finish in time")
	}
}

func TestWaitForTrialVerdictIsANoOpWithoutATrial(t *testing.T) {
	if err := waitForTrialVerdict(context.Background(), time.Millisecond, func() { t.Fatal("no wait expected") }); err != nil {
		t.Fatal(err)
	}
}

func TestGetAgentVersionReportsTheSupervision(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{
		Role: supervisor.RoleImage, PID: os.Getpid(), ImageVersion: "2026.10.29-210857",
		UpdateState: supervisor.UpdateRejected, Reason: "did not pass its checks within 3m0s", UpdateSHA256: "abc",
	})
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()

	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sup := resp.GetSupervision()
	if sup.GetRole() != "image" || sup.GetImageVersion() != "2026.10.29-210857" ||
		sup.GetUpdateState() != "rejected" || sup.GetReason() == "" || sup.GetUpdateSha256() != "abc" {
		t.Fatalf("supervision = %+v", sup)
	}
}

func TestGetAgentVersionOmitsTheSupervisionWithoutASupervisor(t *testing.T) {
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Supervision != nil {
		t.Fatalf("an unsupervised agent must not report supervision, got %+v", resp.Supervision)
	}
}

func TestSupervisedUpdateIsRefusedWhileUpdatesArePaused(t *testing.T) {
	env := superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleImage, PID: os.Getpid(), UpdatesPaused: "agent updates are off: the emergency flag is set"})
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	data, sha := uploadFixture()

	stream, _ := client.UpdateAgent(context.Background())
	err := sendV1UpdateStream(t, stream, data, sha, nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "emergency flag") {
		t.Fatalf("want the pause reason, got %v", err)
	}
	if entries, _ := os.ReadDir(env.inbox); len(entries) != 0 {
		t.Fatal("nothing may be staged while updates are paused")
	}
}

func TestUpdateOSIsRefusedDuringAnAutomaticTrial(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid(), Automatic: true})
	start := time.Now()
	msg := updateOSV1(t, func(svc *AgentService) { svc.isWendyOSHost = func() bool { return true } })
	if msg != automaticTrialMessage {
		t.Fatalf("error message = %q", msg)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the refusal must be immediate, not after the trial")
	}
}

func TestGetAgentVersionReportsAStagedUploadAsPending(t *testing.T) {
	superviseForTest(t, nil)
	data, sha := uploadFixture()
	// The same agent was rejected before, for instance by an automatic update.
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleImage, PID: os.Getpid(),
		UpdateState: supervisor.UpdateRejected, UpdateSHA256: sha, Reason: "the new agent has not reached the cloud yet"})
	client, _, cleanup := startAgentServerForUpdate(t, nil)
	defer cleanup()
	stream, err := client.UpdateAgent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sendV1UpdateStream(t, stream, data, sha, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	resp, err := client.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if sup := resp.GetSupervision(); sup.GetUpdateState() != "trial" || sup.GetUpdateSha256() != sha || sup.GetReason() != "" {
		t.Fatalf("until this agent exits, the client must not read the old verdict as its own: %+v", sup)
	}
}

// osUpdateV2Stream captures what the v2 UpdateOS handler sends.
type osUpdateV2Stream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*agentpbv2.UpdateOSResponse
}

func (s *osUpdateV2Stream) Send(r *agentpbv2.UpdateOSResponse) error {
	s.sent = append(s.sent, r)
	return nil
}
func (s *osUpdateV2Stream) Context() context.Context { return s.ctx }

func TestUpdateOSV2IsRefusedDuringAnAutomaticTrial(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid(), Automatic: true})
	svc := NewOSUpdateService(zap.NewNop(), &AgentInstaller{})
	svc.isWendyOSHost = func() bool { t.Error("the trial gate must come first"); return true }
	stream := &osUpdateV2Stream{ctx: context.Background()}
	if err := svc.UpdateOS(&agentpbv2.UpdateOSRequest{ArtifactUrl: "https://example.com/os.wendy"}, stream); err != nil {
		t.Fatal(err)
	}
	last := stream.sent[len(stream.sent)-1]
	if got := last.GetFailed().GetErrorMessage(); got != automaticTrialMessage {
		t.Fatalf("want the automatic-trial refusal, got %+v", last)
	}
}

func TestHoldSupervisorWakeNamesThisAgentUntilReleased(t *testing.T) {
	superviseForTest(t, nil)
	dir := t.TempDir()
	orig := supervisorMarkDir
	supervisorMarkDir = dir
	t.Cleanup(func() { supervisorMarkDir = orig })
	updating := supervisor.UpdatingFunc(dir)

	release := holdSupervisorWake()
	if !updating(os.Getpid()) {
		t.Fatal("the supervisor must see this agent installing")
	}
	release()
	if updating(os.Getpid()) {
		t.Fatal("the mark must go when the OS update returns")
	}
}

func TestHoldSupervisorWakeNests(t *testing.T) {
	superviseForTest(t, nil)
	dir := t.TempDir()
	orig := supervisorMarkDir
	supervisorMarkDir = dir
	t.Cleanup(func() { supervisorMarkDir = orig })
	updating := supervisor.UpdatingFunc(dir)

	upload, osUpdate := holdSupervisorWake(), holdSupervisorWake()
	upload()
	upload() // a second release of the same hold changes nothing
	if !updating(os.Getpid()) {
		t.Fatal("the OS update still holds the mark")
	}
	osUpdate()
	if updating(os.Getpid()) {
		t.Fatal("the mark must go with the last hold")
	}
}
