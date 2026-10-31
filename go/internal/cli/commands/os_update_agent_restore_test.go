package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// imageAgent is the agent a freshly booted OS image runs. An uploaded agent
// replaces it; the test payloads are version strings, so it then reports that.
type imageAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	mu       sync.Mutex
	version  string
	uploaded string
	dropAck  bool // restart before acknowledging, as a real agent may
}

func (s *imageAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &agentpb.GetAgentVersionResponse{Version: s.version, Os: "linux", CpuArchitecture: "arm64"}, nil
}

func (s *imageAgent) UpdateAgent(stream grpc.BidiStreamingServer[agentpb.UpdateAgentRequest, agentpb.UpdateAgentResponse]) error {
	var data []byte
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if chunk := req.GetChunk(); chunk != nil {
			data = append(data, chunk.GetData()...)
			continue
		}
		if req.GetControl().GetUpdate() != nil {
			s.mu.Lock()
			s.uploaded, s.version = string(data), string(data)
			s.mu.Unlock()
			if s.dropAck {
				return status.Error(codes.Unavailable, "transport is closing")
			}
			return stream.Send(&agentpb.UpdateAgentResponse{ResponseType: &agentpb.UpdateAgentResponse_Updated_{
				Updated: &agentpb.UpdateAgentResponse_Updated{},
			}})
		}
	}
}

func (s *imageAgent) received() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploaded
}

// serveImageAgent returns a connection whose Reconnect reaches agent, the way
// a reconnect after the OS reboot reaches the new image's agent.
func serveImageAgent(t *testing.T, agent *imageAgent) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, agent)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	var dial func(context.Context) (*grpcclient.AgentConnection, error)
	dial = func(context.Context) (*grpcclient.AgentConnection, error) {
		cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return &grpcclient.AgentConnection{
			Conn:         cc,
			AgentService: agentpb.NewWendyAgentServiceClient(cc),
			Reconnect:    dial,
		}, nil
	}
	return &grpcclient.AgentConnection{Reconnect: dial}
}

// The latest agent on each channel, as the release lookups would report it.
func stubAgentReleases(t *testing.T) {
	t.Helper()
	origVersion, origArtifact := resolveAgentVersionFn, resolveAgentArtifactFn
	t.Cleanup(func() { resolveAgentVersionFn, resolveAgentArtifactFn = origVersion, origArtifact })

	latest := func(nightly bool) string {
		if nightly {
			return "2026.10.27-102715"
		}
		return "2026.10.27-091653"
	}
	resolveAgentVersionFn = func(nightly bool) (string, string, error) {
		return latest(nightly), "gcs", nil
	}
	resolveAgentArtifactFn = func(_, _ string, nightly bool) ([]byte, string, string, error) {
		return []byte(latest(nightly)), latest(nightly), "gcs", nil
	}
}

// An A/B update boots a slot whose agent was frozen when the image was built,
// so the agent installed before the update is gone after it.
func TestRestoreAgentAfterOSUpdate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nightly      bool
		imageAgent   string
		dropAck      bool
		wantUploaded string
	}{
		{
			name:         "nightly image shipped an older stable agent",
			nightly:      true,
			imageAgent:   "2026.10.25-191108",
			wantUploaded: "2026.10.27-102715",
		},
		{
			name:       "nightly image already runs the latest nightly agent",
			nightly:    true,
			imageAgent: "2026.10.27-102715",
		},
		{
			name:         "stable image shipped an older agent",
			imageAgent:   "2026.10.25-191108",
			wantUploaded: "2026.10.27-091653",
		},
		{
			name:       "stable image shipped a newer agent than the latest release",
			imageAgent: "2026.10.28-080000",
		},
		{
			name:         "agent restarts before acknowledging the upload",
			nightly:      true,
			imageAgent:   "2026.10.25-191108",
			dropAck:      true,
			wantUploaded: "2026.10.27-102715",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubAgentReleases(t)
			agent := &imageAgent{version: tc.imageAgent, dropAck: tc.dropAck}

			if err := restoreAgentAfterOSUpdate(context.Background(), serveImageAgent(t, agent), tc.nightly); err != nil {
				t.Fatalf("restoreAgentAfterOSUpdate: %v", err)
			}
			if got := agent.received(); got != tc.wantUploaded {
				t.Errorf("device received agent %q, want %q", got, tc.wantUploaded)
			}
		})
	}
}

// The OS is committed by then, so the error points at the command that
// finishes the job on the same channel rather than at a second OS update.
func TestRestoreAgentAfterOSUpdateFailureNamesTheRerun(t *testing.T) {
	cause := errors.New("device unreachable")
	for _, tc := range []struct {
		nightly  bool
		wantHint string
	}{
		{true, "'wendy device update --nightly'"},
		{false, "'wendy device update'"},
	} {
		prior := &grpcclient.AgentConnection{Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) {
			return nil, cause
		}}
		err := restoreAgentAfterOSUpdate(context.Background(), prior, tc.nightly)
		if !errors.Is(err, cause) {
			t.Fatalf("nightly=%v: restoreAgentAfterOSUpdate() = %v, want it to wrap %v", tc.nightly, err, cause)
		}
		if !strings.Contains(err.Error(), tc.wantHint) {
			t.Errorf("nightly=%v: restoreAgentAfterOSUpdate() = %q, want it to name %s", tc.nightly, err, tc.wantHint)
		}
	}
}

// An offline update from a local artifact must still succeed; the agent is
// left for later, and the user is told how.
func TestRestoreAgentAfterOSUpdateWithoutReleaseLookup(t *testing.T) {
	stubAgentReleases(t)
	resolveAgentVersionFn = func(bool) (string, string, error) { return "", "", errors.New("no route to host") }
	agent := &imageAgent{version: "2026.10.25-191108"}

	var err error
	out := captureStdout(t, func() {
		err = restoreAgentAfterOSUpdate(context.Background(), serveImageAgent(t, agent), true)
	})
	if err != nil {
		t.Fatalf("restoreAgentAfterOSUpdate: %v", err)
	}
	if got := agent.received(); got != "" {
		t.Errorf("device received agent %q, want none", got)
	}
	if !strings.Contains(out, "'wendy device update --nightly'") {
		t.Errorf("output %q does not name 'wendy device update --nightly'", out)
	}
}
