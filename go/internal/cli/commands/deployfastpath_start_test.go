package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A detached CLI must consume the acknowledgement before its connection closes.
// These cases reproduce the unchanged stopped-app path and the ordinary start
// path, including output arriving first and EOF without an acknowledgement.
func TestDetachedRunRequiresStarted(t *testing.T) {
	output := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("pre-start output\n")}}}
	started := &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}
	for _, fast := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			frames  []*agentpb.RunContainerLayersResponse
			rpcErr  error
			wantErr bool
		}{
			{"confirmed", []*agentpb.RunContainerLayersResponse{started}, nil, false},
			{"output before confirmation", []*agentpb.RunContainerLayersResponse{output, started}, nil, false},
			{"closed without confirmation", nil, nil, true},
			{"output then closed", []*agentpb.RunContainerLayersResponse{output}, nil, true},
			{"RPC failed", nil, errors.New("start refused"), true},
		} {
			mode := "ordinary/"
			if fast {
				mode = "unchanged/"
			}
			t.Run(mode+tc.name, func(t *testing.T) {
				isolateFingerprintCache(t)
				const appID, deviceKey, inputHash, layerID = "start-confirmation-app", "testdevice", "input", "layer"
				stream := &topStartStream{responses: append([]*agentpb.RunContainerLayersResponse(nil), tc.frames...)}
				client := &fastPathContainerClient{appName: appID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{layerID: true}, startStream: stream, startErr: tc.rpcErr}
				conn := &grpcclient.AgentConnection{Host: "localhost", ContainerService: client}
				cfg := &appconfig.AppConfig{AppID: appID}
				var err error
				if fast {
					saveDeployFingerprint(appID, deviceKey, deployFingerprint{InputHash: inputHash, LayerDiffIDs: []string{layerID}})
					var handled bool
					handled, err = tryDeployFastPath(context.Background(), conn, cfg, deviceKey, inputHash, runOptions{detach: true})
					if !handled {
						t.Fatal("verified lifecycle failure must not trigger a rebuild")
					}
				} else {
					err = startExistingContainer(context.Background(), conn, cfg, runOptions{detach: true})
				}
				if (err != nil) != tc.wantErr {
					t.Fatalf("error = %v, want error %v", err, tc.wantErr)
				}
				if client.startCalls != 1 {
					t.Fatalf("start calls = %d", client.startCalls)
				}
				if len(stream.responses) != 0 {
					t.Fatal("returned before consuming Started")
				}
			})
		}
	}
}
