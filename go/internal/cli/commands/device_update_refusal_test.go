package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const refusalRelease = "2026.11.04-081952"

// refusingAgent is a supervised device: an upload is refused with refuse, or
// otherwise accepted and approved, with the stream dropped as the agent restarts.
type refusingAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	mu       sync.Mutex
	refuse   error
	sha      string
	approved bool
}

func (a *refusingAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	resp := &agentpb.GetAgentVersionResponse{Version: "2026.10.01-000000", CpuArchitecture: "arm64", Os: "linux",
		Supervision: &agentpb.AgentSupervision{Role: "approved"}}
	if a.approved {
		resp.Version = refusalRelease
		resp.Supervision.UpdateState, resp.Supervision.UpdateSha256 = "approved", a.sha
	}
	return resp, nil
}

func (a *refusingAgent) UpdateAgent(stream grpc.BidiStreamingServer[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "no update command")
		}
		if err != nil {
			return err
		}
		if u := msg.GetControl().GetUpdate(); u != nil {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.refuse != nil {
				return a.refuse
			}
			a.sha, a.approved = u.GetSha256(), true
			return status.Error(codes.Unavailable, "agent restarting")
		}
	}
}

type deviceUpdateRun struct {
	out      string
	err      error
	osStep   bool
	osStepOn *grpcclient.AgentConnection
}

func runDeviceUpdate(t *testing.T, agent *refusingAgent, json bool, args ...string) deviceUpdateRun {
	t.Helper()
	dir, err := os.MkdirTemp("", "wdyup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, agent)
	go srv.Serve(l)
	t.Cleanup(srv.Stop)

	prevChoice, prevDevice, prevJSON := connectSimulatorChoiceFn, deviceFlag, jsonOutput
	prevVersion, prevArtifact, prevOS := resolveAgentVersionFn, resolveAgentArtifactFn, maybeCheckOSUpdateFn
	t.Cleanup(func() {
		connectSimulatorChoiceFn, deviceFlag, jsonOutput = prevChoice, prevDevice, prevJSON
		resolveAgentVersionFn, resolveAgentArtifactFn, maybeCheckOSUpdateFn = prevVersion, prevArtifact, prevOS
	})
	fastVerdict(t)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	deviceFlag, jsonOutput = "vm:robot", json
	connectSimulatorChoiceFn = func(ctx context.Context, _ *simulatorChoice, _ bool) (*SelectedDevice, error) {
		c, err := grpcclient.ConnectUnix(ctx, sock)
		if err != nil {
			return nil, err
		}
		c.Reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			return grpcclient.ConnectUnix(ctx, sock)
		}
		return &SelectedDevice{Agent: c}, nil
	}
	resolveAgentVersionFn = func(bool) (string, string, error) { return refusalRelease, "test", nil }
	resolveAgentArtifactFn = func(string, string, bool) ([]byte, string, string, error) {
		return []byte("release agent"), refusalRelease, "test", nil
	}
	var run deviceUpdateRun
	maybeCheckOSUpdateFn = func(_ context.Context, _ *agentpb.GetAgentVersionResponse, conn *grpcclient.AgentConnection, _, _ bool, _ string, _ int) (osUpdateOutcome, error) {
		run.osStep, run.osStepOn = true, conn
		if _, err := conn.AgentService.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{}); err != nil {
			t.Errorf("the OS step got a dead connection: %v", err)
		}
		return osUpdateOutcome{}, nil
	}

	cmd := newDeviceUpdateCmd()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd.SetContext(ctx)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	run.out = captureStdout(t, func() { run.err = cmd.RunE(cmd, nil) })
	return run
}

var errContractRefusal = status.Error(codes.FailedPrecondition, contractRefusal)

func localBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wendy-agent")
	if err := os.WriteFile(path, []byte("dev agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeviceUpdateGoesOnToTheOSStepAfterACheckRefusal(t *testing.T) {
	run := runDeviceUpdate(t, &refusingAgent{refuse: errContractRefusal}, false)
	if run.err != nil || !run.osStep {
		t.Fatalf("want the OS step after the refusal, got err %v, OS step %v\n%s", run.err, run.osStep, run.out)
	}
	if !strings.Contains(run.out, "the device kept agent 2026.10.01-000000") {
		t.Fatalf("the user must learn which agent stays:\n%s", run.out)
	}
}

func TestDeviceUpdateJSONReportsACheckRefusalAsRejected(t *testing.T) {
	run := runDeviceUpdate(t, &refusingAgent{refuse: errContractRefusal}, true)
	var refused *agentCheckRefusedError
	if !errors.As(run.err, &refused) || !strings.Contains(run.out, `"status":"rejected"`) || run.osStep {
		t.Fatalf("want a rejected object and a non-zero exit, got err %v, OS step %v\n%s", run.err, run.osStep, run.out)
	}
}

func TestDeviceUpdateStopsWhenTheDeviceRefusesTheBinary(t *testing.T) {
	for _, json := range []bool{false, true} {
		run := runDeviceUpdate(t, &refusingAgent{refuse: errContractRefusal}, json, "--binary", localBinary(t))
		var refused *agentCheckRefusedError
		if !errors.As(run.err, &refused) || run.osStep {
			t.Fatalf("json=%v: a refused --binary must fail, got err %v, OS step %v", json, run.err, run.osStep)
		}
		if json != strings.Contains(run.out, `"status":"rejected"`) {
			t.Fatalf("json=%v: output\n%s", json, run.out)
		}
	}
}

func TestDeviceUpdateStopsWhenTheDeviceIsBusy(t *testing.T) {
	busy := status.Error(codes.FailedPrecondition, "an agent update is being verified; retry in a few minutes")
	run := runDeviceUpdate(t, &refusingAgent{refuse: busy}, false)
	var refused *agentCheckRefusedError
	if run.err == nil || errors.As(run.err, &refused) || run.osStep {
		t.Fatalf("busy must stay fatal, got err %v, OS step %v", run.err, run.osStep)
	}
}

func TestDeviceUpdateStillVerifiesAnUnconfirmedUpload(t *testing.T) {
	run := runDeviceUpdate(t, &refusingAgent{}, false)
	if run.err != nil || !run.osStep || !strings.Contains(run.out, "Agent updated successfully (agent reports "+refusalRelease+")") {
		t.Fatalf("want a verified update and the OS step, got err %v, OS step %v\n%s", run.err, run.osStep, run.out)
	}
}
