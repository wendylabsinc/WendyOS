package commands

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deploymentStartResultStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	responses  []*agentpb.RunContainerLayersResponse
	recvErr    error
	beforeRecv func()
}

func (s *deploymentStartResultStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if s.beforeRecv != nil {
		s.beforeRecv()
	}
	if len(s.responses) > 0 {
		response := s.responses[0]
		s.responses = s.responses[1:]
		return response, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

// A detached CLI must consume the acknowledgement before its connection closes.
// These cases reproduce the unchanged stopped-app path and the ordinary start
// path, including output arriving first and EOF without an acknowledgement.
func TestDetachedRunRequiresStarted(t *testing.T) {
	output := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("pre-start output\n")}}}
	stderr := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StderrOutput{StderrOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("pre-start error output\n")}}}
	started := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}
	for _, fast := range []bool{false, true} {
		for _, tc := range []struct {
			name          string
			frames        []*agentpb.RunContainerLayersResponse
			rpcErr        error
			recvErr       error
			wantErr       bool
			wantRemaining int
		}{
			{name: "confirmed", frames: []*agentpb.RunContainerLayersResponse{started}},
			{name: "output before confirmation", frames: []*agentpb.RunContainerLayersResponse{output, stderr, started}},
			{name: "returns at confirmation", frames: []*agentpb.RunContainerLayersResponse{started, output}, wantRemaining: 1},
			{name: "closed without confirmation", wantErr: true},
			{name: "output then closed", frames: []*agentpb.RunContainerLayersResponse{output}, wantErr: true},
			{name: "RPC failed", rpcErr: errors.New("start refused"), wantErr: true},
			{name: "receive failed", recvErr: status.Error(codes.PermissionDenied, "start denied"), wantErr: true},
			{name: "output then receive failed", frames: []*agentpb.RunContainerLayersResponse{output}, recvErr: status.Error(codes.Unavailable, "connection lost"), wantErr: true},
			{name: "receive cancelled", recvErr: context.Canceled, wantErr: true},
			{name: "receive deadline exceeded", recvErr: context.DeadlineExceeded, wantErr: true},
		} {
			mode := "ordinary/"
			if fast {
				mode = "unchanged/"
			}
			t.Run(mode+tc.name, func(t *testing.T) {
				isolateFingerprintCache(t)
				const appID, deviceKey, inputHash, layerID = "start-confirmation-app", "testdevice", "input", "layer"
				stream := &deploymentStartResultStream{responses: append([]*agentpb.RunContainerLayersResponse(nil), tc.frames...), recvErr: tc.recvErr}
				client := &fastPathContainerClient{appName: appID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{layerID: true}, startStream: stream, startErr: tc.rpcErr}
				conn := &grpcclient.AgentConnection{Host: "localhost", ContainerService: client}
				cfg := &appconfig.AppConfig{AppID: appID}
				starts := 0
				opts := runOptions{detach: true, onDeployStarted: func() { starts++ }}
				var err error
				if fast {
					saveDeployFingerprint(appID, deviceKey, deployFingerprint{InputHash: inputHash, LayerDiffIDs: []string{layerID}})
					var handled bool
					handled, err = tryDeployFastPath(context.Background(), conn, cfg, deviceKey, inputHash, opts)
					if !handled {
						t.Fatal("verified lifecycle failure must not trigger a rebuild")
					}
				} else {
					err = startExistingContainer(context.Background(), conn, cfg, opts)
				}
				if (err != nil) != tc.wantErr {
					t.Fatalf("error = %v, want error %v", err, tc.wantErr)
				}
				for _, cause := range []error{tc.rpcErr, tc.recvErr} {
					if cause != nil && !errors.Is(err, cause) {
						t.Fatalf("error = %v, want wrapped cause %v", err, cause)
					}
				}
				wantStarts := 1
				if tc.wantErr {
					wantStarts = 0
				}
				if starts != wantStarts {
					t.Fatalf("start notifications = %d, want %d", starts, wantStarts)
				}
				if client.startCalls != 1 {
					t.Fatalf("start calls = %d", client.startCalls)
				}
				if len(stream.responses) != tc.wantRemaining {
					t.Fatalf("remaining frames = %d, want %d", len(stream.responses), tc.wantRemaining)
				}
			})
		}
	}
}

func TestDetachedRunWaitsForDelayedStarted(t *testing.T) {
	for _, mode := range []string{"ordinary", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			isolateFingerprintCache(t)
			const appID, deviceKey, inputHash, layerID = "delayed-start-app", "device", "input", "layer"
			saveDeployFingerprint(appID, deviceKey, deployFingerprint{InputHash: inputHash, LayerDiffIDs: []string{layerID}})
			waiting := make(chan struct{})
			release := make(chan struct{})
			var starts atomic.Int32
			stream := &deploymentStartResultStream{responses: []*agentpb.RunContainerLayersResponse{
				{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("starting\n")}}},
				detachedStarted(),
			}}
			stream.beforeRecv = func() {
				if len(stream.responses) == 1 {
					close(waiting)
					<-release
				}
			}
			client := &fastPathContainerClient{appName: appID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{layerID: true}, startStream: stream}
			conn := &grpcclient.AgentConnection{ContainerService: client}
			cfg := &appconfig.AppConfig{AppID: appID}
			opts := runOptions{detach: true, onDeployStarted: func() { starts.Add(1) }}
			type result struct {
				handled bool
				err     error
			}
			done := make(chan result, 1)
			// Always unblock and join the worker before test cleanup restores globals.
			workerExited := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
				<-workerExited
			}()
			go func() {
				defer close(workerExited)
				if mode == "unchanged" {
					handled, err := tryDeployFastPath(context.Background(), conn, cfg, deviceKey, inputHash, opts)
					done <- result{handled, err}
				} else {
					done <- result{true, startExistingContainer(context.Background(), conn, cfg, opts)}
				}
			}()
			select {
			case <-waiting:
			case got := <-done:
				t.Fatalf("returned before requesting acknowledgement: %+v", got)
			case <-time.After(5 * time.Second):
				t.Fatal("did not request acknowledgement")
			}
			select {
			case got := <-done:
				t.Fatalf("returned while acknowledgement was delayed: %+v", got)
			case <-time.After(50 * time.Millisecond):
			}
			if starts.Load() != 0 {
				t.Fatal("reported start before acknowledgement")
			}
			close(release)
			select {
			case got := <-done:
				if !got.handled || got.err != nil || starts.Load() != 1 {
					t.Fatalf("handled=%v err=%v notifications=%d", got.handled, got.err, starts.Load())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("did not return after acknowledgement")
			}
		})
	}
}
