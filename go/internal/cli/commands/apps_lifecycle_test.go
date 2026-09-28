package commands

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// appsFakeContainerClient records lifecycle requests. StartContainer confirms
// the start and then streams output; AttachContainer is unsupported, so an
// attached start falls back to StartContainer; ListContainers reports
// container.
type appsFakeContainerClient struct {
	agentpb.WendyContainerServiceClient // embedded nil: unexpected calls panic

	container *agentpb.AppContainer
	output    string
	startReq  *agentpb.StartContainerRequest
	stopReq   *agentpb.StopContainerRequest
	deleteReq *agentpb.DeleteContainerRequest
}

func (f *appsFakeContainerClient) StartContainer(_ context.Context, in *agentpb.StartContainerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	f.startReq = in
	frames := []*agentpb.RunContainerLayersResponse{{
		ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}},
	}}
	if f.output != "" {
		frames = append(frames, &agentpb.RunContainerLayersResponse{
			ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{
				StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte(f.output)},
			},
		})
	}
	return &appsFakeStartStream{frames: frames}, nil
}

func (f *appsFakeContainerClient) AttachContainer(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse], error) {
	return nil, status.Error(codes.Unimplemented, "attach is not supported by this fake")
}

func (f *appsFakeContainerClient) StopContainer(_ context.Context, in *agentpb.StopContainerRequest, _ ...grpc.CallOption) (*agentpb.StopContainerResponse, error) {
	f.stopReq = in
	return &agentpb.StopContainerResponse{}, nil
}

func (f *appsFakeContainerClient) DeleteContainer(_ context.Context, in *agentpb.DeleteContainerRequest, _ ...grpc.CallOption) (*agentpb.DeleteContainerResponse, error) {
	f.deleteReq = in
	return &agentpb.DeleteContainerResponse{}, nil
}

func (f *appsFakeContainerClient) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	return &fakeListContainersStream{resp: &agentpb.ListContainersResponse{Container: f.container}}, nil
}

type appsFakeStartStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse] // embedded nil
	frames                                                         []*agentpb.RunContainerLayersResponse
}

func (s *appsFakeStartStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	next := s.frames[0]
	s.frames = s.frames[1:]
	return next, nil
}

// useAppsFake points the apps commands at fake and sets JSON mode to json.
func useAppsFake(t *testing.T, fake *appsFakeContainerClient, json bool) {
	t.Helper()
	prevResolve, prevJSON := resolveAppsTargetFn, jsonOutput
	resolveAppsTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{ContainerService: fake}}, nil
	}
	jsonOutput = json
	t.Cleanup(func() { resolveAppsTargetFn, jsonOutput = prevResolve, prevJSON })
}

// runAppsCmd executes cmd and returns what it wrote to the process's stdout
// and stderr (the apps commands write there directly, not to cmd.Out).
func runAppsCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd.SetArgs(args)
	stdout = captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err = cmd.ExecuteContext(context.Background())
		})
	})
	return stdout, stderr, err
}

func decodeAppResult(t *testing.T, stdout string) appActionResult {
	t.Helper()
	var got appActionResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	return got
}

func TestAppsStartDetachedJSONReportsTheRestartPolicy(t *testing.T) {
	fake := &appsFakeContainerClient{}
	useAppsFake(t, fake, true)

	stdout, _, err := runAppsCmd(t, newAppsStartCmd(), "--detach", "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := appActionResult{App: "demo", Action: "start", Status: "started", RestartPolicy: "unless-stopped"}
	if got := decodeAppResult(t, stdout); !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if mode := fake.startReq.GetRestartPolicy().GetMode(); mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("detached start sent restart policy %v, want UNLESS_STOPPED (documented behaviour)", mode)
	}
}

func TestAppsStartAttachedJSONKeepsStdoutForTheResult(t *testing.T) {
	fake := &appsFakeContainerClient{
		output: "hello from the app\n",
		container: &agentpb.AppContainer{
			AppName:           "demo",
			RunningState:      agentpb.AppRunningState_STOPPED,
			TerminationReason: "exited",
		},
	}
	useAppsFake(t, fake, true)

	stdout, stderr, err := runAppsCmd(t, newAppsStartCmd(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	want := appActionResult{App: "demo", Action: "start", Status: "stopped", ExitCode: &zero, TerminationReason: "exited"}
	if got := decodeAppResult(t, stdout); !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if !strings.Contains(stderr, "hello from the app") {
		t.Errorf("the app's own output must go to stderr in JSON mode; stderr = %q", stderr)
	}
	if fake.startReq.GetRestartPolicy() != nil {
		t.Error("an attached start must keep the app's deployed restart policy")
	}
}

func TestAppsStartHelpNamesTheDetachRestartPolicy(t *testing.T) {
	cmd := newAppsStartCmd()
	if usage := cmd.Flags().Lookup("detach").Usage; !strings.Contains(usage, "unless-stopped") {
		t.Errorf("--detach usage = %q, want it to name the unless-stopped restart policy", usage)
	}
	if !strings.Contains(cmd.Long, "unless-stopped") || !strings.Contains(cmd.Long, "wendy device apps stop") {
		t.Errorf("start help = %q, want the restart-policy effect of --detach explained", cmd.Long)
	}
}

func TestAppsStopJSONAndText(t *testing.T) {
	fake := &appsFakeContainerClient{}
	useAppsFake(t, fake, true)
	stdout, _, err := runAppsCmd(t, newAppsStopCmd(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodeAppResult(t, stdout), (appActionResult{App: "demo", Action: "stop", Status: "stopped"}); !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if fake.stopReq.GetAppName() != "demo" {
		t.Errorf("StopContainer app = %q, want demo", fake.stopReq.GetAppName())
	}

	useAppsFake(t, &appsFakeContainerClient{}, false)
	stdout, stderr, err := runAppsCmd(t, newAppsStopCmd(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || !strings.Contains(stderr, "Application demo stopped.") {
		t.Errorf("text mode changed: stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestAppsRemoveWithoutTerminalNeedsForce(t *testing.T) {
	stubNonInteractive(t)
	prev := resolveAppsTargetFn
	resolveAppsTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		t.Fatal("remove connected to a device before refusing")
		return nil, nil
	}
	t.Cleanup(func() { resolveAppsTargetFn = prev })

	_, _, err := runAppsCmd(t, newAppsRemoveCmd(), "demo")
	if !IsUsageError(err) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("err = %q, want it to name --force", err)
	}
	if steps := NextSteps(err); len(steps) != 1 || !strings.HasSuffix(steps[0], "remove demo --force") {
		t.Errorf("next steps = %q, want the exact command to run", steps)
	}
}

func TestAppsRemoveForceJSON(t *testing.T) {
	stubNonInteractive(t)
	fake := &appsFakeContainerClient{}
	useAppsFake(t, fake, true)

	stdout, _, err := runAppsCmd(t, newAppsRemoveCmd(), "--force", "--cleanup", "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := appActionResult{App: "demo", Action: "remove", Status: "removed", DeleteImage: true}
	if got := decodeAppResult(t, stdout); !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if !fake.deleteReq.GetDeleteImage() || fake.deleteReq.GetDeleteVolumes() {
		t.Errorf("DeleteContainer request = %v, want image deleted and volumes kept", fake.deleteReq)
	}
}
