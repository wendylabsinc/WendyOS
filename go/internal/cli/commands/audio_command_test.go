package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func TestShouldOpenAudioSetDefaultTUI(t *testing.T) {
	tests := []struct {
		name        string
		idSet       bool
		interactive bool
		json        bool
		want        bool
	}{
		{name: "interactive without id", interactive: true, want: true},
		{name: "interactive with id", idSet: true, interactive: true},
		{name: "non-interactive without id"},
		{name: "json on interactive terminal", interactive: true, json: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldOpenAudioSetDefaultTUI(tt.idSet, tt.interactive, tt.json); got != tt.want {
				t.Fatalf("shouldOpenAudioSetDefaultTUI(%v, %v, %v) = %v, want %v", tt.idSet, tt.interactive, tt.json, got, tt.want)
			}
		})
	}
}

func TestAudioSetDefaultStillRequiresIDNonInteractively(t *testing.T) {
	originalInteractive := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = originalInteractive })

	cmd := newAudioSetDefaultCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "id" not set`) {
		t.Fatalf("error = %v, want missing id error", err)
	}
}

type audioListenCommandClient struct {
	agentpb.WendyAudioServiceClient
	request *agentpb.StreamAudioRequest
	err     error
}

func (c *audioListenCommandClient) ListAudioDevices(context.Context, *agentpb.ListAudioDevicesRequest, ...grpc.CallOption) (*agentpb.ListAudioDevicesResponse, error) {
	return &agentpb.ListAudioDevicesResponse{Devices: []*agentpb.AudioDevice{
		mkDev(11, "front", "Front microphone", inT),
		mkDev(12, "rear", "Rear microphone", inT),
	}}, nil
}

func (c *audioListenCommandClient) StreamAudio(_ context.Context, req *agentpb.StreamAudioRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.AudioChunk], error) {
	c.request = req
	return nil, c.err
}

func TestAudioListenNonInteractiveDoesNotOpenPicker(t *testing.T) {
	stubInteractive(t)
	streamErr := errors.New("test stream unavailable")
	client := &audioListenCommandClient{err: streamErr}
	previous := connectAudioListenFn
	connectAudioListenFn = func(_ context.Context, opts ...resolveOption) (*grpcclient.AgentConnection, error) {
		var cfg resolveConfig
		for _, opt := range opts {
			opt(&cfg)
		}
		if !cfg.nonInteractive || !cfg.suppressUpdateCheck {
			t.Fatalf("background connection options = %+v", cfg)
		}
		return &grpcclient.AgentConnection{AudioService: client}, nil
	}
	t.Cleanup(func() { connectAudioListenFn = previous })
	cmd := newAudioListenCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--non-interactive"})
	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, streamErr) {
		t.Fatalf("error = %v, want stream error", err)
	}
	if client.request == nil || client.request.GetDeviceId() != 11 {
		t.Fatalf("stream request = %v, want auto-selected microphone 11", client.request)
	}
}
