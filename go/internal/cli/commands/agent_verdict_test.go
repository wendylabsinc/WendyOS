package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scriptedVersionClient answers GetAgentVersion from a script, one step per call.
type scriptedVersionClient struct {
	agentpb.WendyAgentServiceClient
	steps []versionStep
	calls int
}

type versionStep struct {
	resp *agentpb.GetAgentVersionResponse
	err  error
}

func (c *scriptedVersionClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	step := c.steps[min(c.calls, len(c.steps)-1)]
	c.calls++
	return step.resp, step.err
}

func supervised(version, role, state, sha, reason string) versionStep {
	return versionStep{resp: &agentpb.GetAgentVersionResponse{Version: version, Supervision: &agentpb.AgentSupervision{
		Role: role, UpdateState: state, UpdateSha256: sha, Reason: reason, ImageVersion: "2026.10.29-210857",
	}}}
}

func fastVerdict(t *testing.T) {
	t.Helper()
	timeout, poll, unknown := agentVerdictTimeout, agentVerdictPoll, agentUnknownUploadGrace
	agentVerdictTimeout, agentVerdictPoll, agentUnknownUploadGrace = 200*time.Millisecond, time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { agentVerdictTimeout, agentVerdictPoll, agentUnknownUploadGrace = timeout, poll, unknown })
}

func noReconnect(context.Context) (*grpcclient.AgentConnection, error) {
	return nil, errors.New("no reconnect expected")
}

func TestAwaitAgentVerdictReturnsAtOnceForAnUnsupervisedDevice(t *testing.T) {
	fastVerdict(t)
	client := &scriptedVersionClient{steps: []versionStep{{resp: &agentpb.GetAgentVersionResponse{Version: "2026.11.02-101010"}}}}
	v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {})
	if err != nil || v.Supervised || client.calls != 1 {
		t.Fatalf("verdict = %+v, err %v, calls %d", v, err, client.calls)
	}
}

func TestAwaitAgentVerdictWaitsForTheApproval(t *testing.T) {
	fastVerdict(t)
	client := &scriptedVersionClient{steps: []versionStep{
		supervised("2026.11.02-101010", "trial", "trial", "new", ""),
		supervised("2026.11.02-101010", "trial", "trial", "new", ""),
		supervised("2026.11.02-101010", "approved", "approved", "new", ""),
	}}
	waits := 0
	v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() { waits++ })
	if err != nil || !v.Supervised || !v.Approved || v.Running != "2026.11.02-101010" {
		t.Fatalf("verdict = %+v, err %v", v, err)
	}
	if waits != 1 {
		t.Fatalf("the wait is announced once, got %d", waits)
	}
}

func TestAwaitAgentVerdictIgnoresAVerdictAboutAnotherAgent(t *testing.T) {
	fastVerdict(t)
	client := &scriptedVersionClient{steps: []versionStep{
		// The previous agent still answers with an earlier upload's rejection.
		supervised("2026.10.30-154553", "approved", "rejected", "older", "an earlier failure"),
		supervised("2026.11.02-101010", "trial", "trial", "new", ""),
		supervised("2026.10.30-154553", "approved", "rejected", "new", "the client that sent the update has not reached the new agent yet"),
	}}
	v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {})
	var rejected *agentRejectedError
	if !errors.As(err, &rejected) || v.Approved || v.Running != "2026.10.30-154553" {
		t.Fatalf("want this upload's rejection, got %+v, %v", v, err)
	}
	if !strings.Contains(err.Error(), "has not reached the new agent") || strings.Contains(err.Error(), "earlier failure") {
		t.Fatalf("error = %v", err)
	}
}

func TestAwaitAgentVerdictReconnectsWhenTheAgentRestarts(t *testing.T) {
	fastVerdict(t)
	dropped := &scriptedVersionClient{steps: []versionStep{{err: errors.New("connection reset")}}}
	after := &scriptedVersionClient{steps: []versionStep{supervised("2026.11.02-101010", "approved", "approved", "new", "")}}
	reconnects := 0
	reconnect := func(context.Context) (*grpcclient.AgentConnection, error) {
		reconnects++
		return &grpcclient.AgentConnection{AgentService: after}, nil
	}
	v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: dropped}, reconnect, "new", func() {})
	if err != nil || !v.Approved || reconnects != 1 {
		t.Fatalf("verdict = %+v, err %v, reconnects %d", v, err, reconnects)
	}
}

func TestAwaitAgentVerdictGivesUp(t *testing.T) {
	fastVerdict(t)
	client := &scriptedVersionClient{steps: []versionStep{supervised("2026.11.02-101010", "trial", "trial", "new", "")}}
	_, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {})
	if err == nil || !strings.Contains(err.Error(), "did not decide") {
		t.Fatalf("want a timeout, got %v", err)
	}
}

func TestAwaitAgentVerdictStopsWhenTheUploadNeverLanded(t *testing.T) {
	fastVerdict(t)
	// The device restarted mid-upload: the agent that answers never staged it.
	client := &scriptedVersionClient{steps: []versionStep{supervised("2026.12.04-100000", "approved", "approved", "older", "")}}
	v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {})
	if err == nil || !strings.Contains(err.Error(), "no record of the new agent") || v.Running != "2026.12.04-100000" {
		t.Fatalf("want a fast failure, got %+v, %v", v, err)
	}
	var rejected *agentRejectedError
	if errors.As(err, &rejected) {
		t.Fatal("a lost upload is not a rejection")
	}
}

func TestAwaitAgentVerdictTrustsAnApprovalOnlyFromTheUploadedBinary(t *testing.T) {
	fastVerdict(t)
	// An earlier approval of this sha, while the device now runs another agent.
	stale := supervised("2026.10.29-210857", "image", "approved", "new", "")
	stale.resp.BinarySha256 = "image-agent"
	client := &scriptedVersionClient{steps: []versionStep{stale}}
	if v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {}); err == nil || v.Approved {
		t.Fatalf("an approval is not this upload's while another binary runs: %+v, %v", v, err)
	}
	ok := supervised("2026.11.02-101010", "approved", "approved", "new", "")
	ok.resp.BinarySha256 = "new"
	client = &scriptedVersionClient{steps: []versionStep{ok}}
	if v, _, err := awaitAgentVerdict(context.Background(), &grpcclient.AgentConnection{AgentService: client}, noReconnect, "new", func() {}); err != nil || !v.Approved {
		t.Fatalf("want the approval, got %+v, %v", v, err)
	}
}

func TestKeptAcrossOSUpdate(t *testing.T) {
	if _, ok := keptAcrossOSUpdate(&agentpb.GetAgentVersionResponse{Version: "2026.11.02-101010", BinarySha256: "mine"}, "mine"); ok {
		t.Fatal("an unsupervised device still needs the re-apply")
	}
	kept := &agentpb.GetAgentVersionResponse{Version: "2026.11.02-101010-dev", BinarySha256: "mine",
		Supervision: &agentpb.AgentSupervision{Role: "trial"}}
	if msg, ok := keptAcrossOSUpdate(kept, "mine"); !ok || !strings.Contains(msg, "kept") {
		t.Fatalf("the supervisor kept the agent: %q, %v", msg, ok)
	}
	// The new image's agent was the same or newer, so the user's is sent again.
	replaced := &agentpb.GetAgentVersionResponse{Version: "2026.11.04-090000", BinarySha256: "image",
		Supervision: &agentpb.AgentSupervision{Role: "image", Reason: "image agent 2026.11.04-090000 is the same or newer than 2026.11.02-101010"}}
	if _, ok := keptAcrossOSUpdate(replaced, "mine"); ok {
		t.Fatal("an agent the image replaced must be applied again")
	}
}

func TestAwaitAgentVerdictStartsWithoutAConnection(t *testing.T) {
	fastVerdict(t)
	// The trial crashed before the CLI reconnected; the previous agent answers.
	fallback := &scriptedVersionClient{steps: []versionStep{
		supervised("2026.10.29-210857", "image", "rejected", "new", "failed 3 times during its trial"),
	}}
	reconnect := func(context.Context) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{AgentService: fallback}, nil
	}
	_, conn, err := awaitAgentVerdict(context.Background(), nil, reconnect, "new", func() {})
	var rejected *agentRejectedError
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "failed 3 times") || conn == nil {
		t.Fatalf("want the supervisor's rejection, got %v (conn %v)", err, conn)
	}
}

const contractRefusal = "new agent failed its checks: this agent is too old to run under the agent supervisor"

func TestAgentUpdateTerminalErrorMarksACheckRefusal(t *testing.T) {
	var refused *agentCheckRefusedError
	if err := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, contractRefusal)); !errors.As(err, &refused) {
		t.Fatalf("a check refusal must be typed, got %T %v", err, err)
	}
	for _, msg := range []string{
		"an update is already in progress",
		"an agent update is being verified; retry in a few minutes",
		"agent updates are off until the agent config is restored",
	} {
		if err := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, msg)); errors.As(err, &refused) {
			t.Fatalf("%q is not a verdict on the agent", msg)
		}
	}
}

func TestPrintRejectedJSONReportsACheckRefusal(t *testing.T) {
	err := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, contractRefusal))
	out := captureStdout(t, func() { printRejectedJSON(err, "2026.11.02-101010") })
	if !strings.Contains(out, `"status":"rejected"`) || !strings.Contains(out, "2026.11.02-101010") || !strings.Contains(out, "too old") {
		t.Fatalf("want a rejected object, got %q", out)
	}
}

func TestCloudBinaryHint(t *testing.T) {
	if got := cloudBinaryHint("./wendy-agent", false); !strings.Contains(got, "Re-run 'wendy device update --binary ./wendy-agent'") {
		t.Fatalf("an unsupervised device needs the re-run, got %q", got)
	}
	got := cloudBinaryHint("./wendy-agent", true)
	if !strings.Contains(got, "wendy device info") || !strings.Contains(got, "if it runs another agent") {
		t.Fatalf("a supervised device may keep the agent itself, got %q", got)
	}
}

func TestAgentVerdictTimeoutCoversAHungTrialAndItsStop(t *testing.T) {
	// Trial deadline, the stop and the sweep after it, then the fallback starting.
	if need := 3*time.Minute + 2*80*time.Second + time.Minute; agentVerdictTimeout < need {
		t.Fatalf("agentVerdictTimeout = %s, want at least %s", agentVerdictTimeout, need)
	}
}

func TestEnsureAgentUpToDateContinuesAfterACheckRefusal(t *testing.T) {
	origResolve, origPerform := resolveAgentVersionFn, performAgentUpdateFn
	t.Cleanup(func() { resolveAgentVersionFn, performAgentUpdateFn = origResolve, origPerform })
	resolveAgentVersionFn = func(bool) (string, string, error) { return "2026.11.04-081952", "test", nil }
	performAgentUpdateFn = func(context.Context, *grpcclient.AgentConnection, string, string, bool) (string, error) {
		return "", agentUpdateTerminalError(status.Error(codes.FailedPrecondition, contractRefusal))
	}
	conn := &grpcclient.AgentConnection{Host: "device"}
	var got *grpcclient.AgentConnection
	var err error
	out := captureStdout(t, func() {
		got, err = ensureAgentUpToDate(context.Background(), conn, &agentpb.GetAgentVersionResponse{Version: "2026.10.01-000000"}, false)
	})
	if err != nil || got != conn {
		t.Fatalf("a refused release must leave the OS update going on the same connection, got %v, %v", got, err)
	}
	if !strings.Contains(out, "2026.10.01-000000") || !strings.Contains(out, "too old") {
		t.Fatalf("the user must learn which agent stays and why, got %q", out)
	}
}

func TestEnsureAgentUpToDateContinuesWhileAgentUpdatesArePaused(t *testing.T) {
	origResolve, origPerform := resolveAgentVersionFn, performAgentUpdateFn
	t.Cleanup(func() { resolveAgentVersionFn, performAgentUpdateFn = origResolve, origPerform })
	resolveAgentVersionFn = func(bool) (string, string, error) { return "2026.11.04-081952", "test", nil }
	for _, refusal := range []string{
		"agent updates are off: the emergency flag is set",
		"agent updates are off until the agent config is restored",
		"an agent update is being verified; retry in a few minutes",
	} {
		performAgentUpdateFn = func(context.Context, *grpcclient.AgentConnection, string, string, bool) (string, error) {
			return "", agentUpdateTerminalError(status.Error(codes.FailedPrecondition, refusal))
		}
		conn := &grpcclient.AgentConnection{Host: "device"}
		var got *grpcclient.AgentConnection
		var err error
		out := captureStdout(t, func() {
			got, err = ensureAgentUpToDate(context.Background(), conn, &agentpb.GetAgentVersionResponse{Version: "2026.10.01-000000"}, false)
		})
		// The OS update is what was asked for; a paused agent update must not block it.
		if err != nil || got != conn {
			t.Fatalf("%q: want the OS update to go on, got %v, %v", refusal, got, err)
		}
		if !strings.Contains(out, refusal) || !strings.Contains(out, "kept agent 2026.10.01-000000") {
			t.Fatalf("%q: the user must learn why and which agent stays, got %q", refusal, out)
		}
	}
}

func TestAgentUpdateTerminalErrorTellsAPauseFromAFailure(t *testing.T) {
	var paused *agentUpdatesPausedError
	for _, msg := range []string{"agent updates are off: /data is not mounted", "an agent update is being verified; retry in a few minutes"} {
		if err := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, msg)); !errors.As(err, &paused) || err.Error() != msg {
			t.Fatalf("%q must read as a pause, got %T %v", msg, err, err)
		}
	}
	if err := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, "an update is already in progress")); errors.As(err, &paused) {
		t.Fatal("a concurrent update is not a pause")
	}
}

func TestUploadSpinnerShowsNoErrorForOutcomesTheCommandGoesOnFrom(t *testing.T) {
	refused := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, contractRefusal))
	unconfirmed := agentUpdateTerminalError(status.Error(codes.Unavailable, "transport is closing"))
	for _, err := range []error{refused, unconfirmed} {
		m, _ := tui.NewSpinner("Uploading agent binary...").Update(uploadSpinnerDone(err))
		model := m.(tui.SpinnerModel)
		if strings.Contains(model.View(), "Error:") {
			t.Fatalf("%v shows as an error: %q", err, model.View())
		}
		if result, _ := model.Result(); result != err {
			t.Fatalf("the outcome must still reach the caller, got %v", result)
		}
	}
	busy := agentUpdateTerminalError(status.Error(codes.FailedPrecondition, "an update is already in progress"))
	if uploadSpinnerDone(busy).Err == nil {
		t.Fatal("a failure must still show as one")
	}
}
