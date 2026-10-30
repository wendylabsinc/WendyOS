package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func progressFrame(phase string, percent int32) *agentpb.UpdateOSResponse {
	return &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Progress_{
		Progress: &agentpb.UpdateOSResponse_Progress{Phase: phase, Percent: percent},
	}}
}

func completedFrame() *agentpb.UpdateOSResponse {
	return &agentpb.UpdateOSResponse{ResponseType: &agentpb.UpdateOSResponse_Completed_{
		Completed: &agentpb.UpdateOSResponse_Completed{RebootRequired: true},
	}}
}

// dropListener remembers accepted connections so a test can cut them mid-stream.
type dropListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *dropListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *dropListener) drop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

// scriptedOSAgent sends frames, then either cuts the connection or holds the
// stream open the way an agent waiting to reboot does.
type scriptedOSAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	frames   []*agentpb.UpdateOSResponse
	cut      func()
	released chan time.Duration
}

func (a *scriptedOSAgent) UpdateOS(_ *agentpb.UpdateOSRequest, stream grpc.ServerStreamingServer[agentpb.UpdateOSResponse]) error {
	for _, f := range a.frames {
		if err := stream.Send(f); err != nil {
			return err
		}
	}
	sent := time.Now()
	if a.cut != nil {
		// Give the frames time to reach the client before the link goes.
		time.Sleep(200 * time.Millisecond)
		a.cut()
		return nil
	}
	select {
	case <-stream.Context().Done():
	case <-time.After(10 * time.Second):
	}
	a.released <- time.Since(sent)
	return nil
}

func startScriptedOSAgent(t *testing.T, frames []*agentpb.UpdateOSResponse, cutAfterFrames bool) (*grpcclient.AgentConnection, *scriptedOSAgent) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lis := &dropListener{Listener: inner}
	agent := &scriptedOSAgent{frames: frames, released: make(chan time.Duration, 1)}
	if cutAfterFrames {
		agent.cut = lis.drop
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient(inner.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})
	return &grpcclient.AgentConnection{Conn: conn, AgentService: agentpb.NewWendyAgentServiceClient(conn)}, agent
}

func nonInteractive(t *testing.T) {
	t.Helper()
	prev := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = prev })
}

func TestStreamOSUpdateReleasesAgentOnCompleted(t *testing.T) {
	nonInteractive(t)
	conn, agent := startScriptedOSAgent(t, []*agentpb.UpdateOSResponse{
		progressFrame("write", 100), progressFrame("done", 100), progressFrame("finalizing", 100), completedFrame(),
	}, false)

	if err := streamOSUpdate(context.Background(), conn, "http://example.invalid/u.wendy", ""); err != nil {
		t.Fatalf("streamOSUpdate: %v", err)
	}
	// The connection stays open, so only a stream cancel can end the agent's hold.
	select {
	case held := <-agent.released:
		if held > 2*time.Second {
			t.Fatalf("agent held the stream %v after Completed; the client never cancelled", held)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent never saw the client cancel after Completed")
	}
}

func TestStreamOSUpdateConnectionLoss(t *testing.T) {
	for _, tc := range []struct {
		name      string
		frames    []*agentpb.UpdateOSResponse
		installed bool
	}{
		{name: "after done", frames: []*agentpb.UpdateOSResponse{progressFrame("write", 100), progressFrame("verify", -1), progressFrame("done", 100)}, installed: true},
		{name: "after finalizing", frames: []*agentpb.UpdateOSResponse{progressFrame("done", 100), progressFrame("finalizing", 100)}, installed: true},
		{name: "after write", frames: []*agentpb.UpdateOSResponse{progressFrame("write", 100)}},
		{name: "after verify", frames: []*agentpb.UpdateOSResponse{progressFrame("write", 100), progressFrame("verify", -1), progressFrame("swap", -1)}},
		{name: "during download", frames: []*agentpb.UpdateOSResponse{progressFrame("downloading", 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nonInteractive(t)
			conn, _ := startScriptedOSAgent(t, tc.frames, true)
			err := streamOSUpdate(context.Background(), conn, "http://example.invalid/u.wendy", "")
			if tc.installed {
				if err != nil {
					t.Fatalf("loss after the install finished returned %v, want nil", err)
				}
				return
			}
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("loss before the install finished returned %v, want the Unavailable error", err)
			}
			if errors.Is(err, errOSUpdateLostAfterInstall) {
				t.Fatalf("loss before the install finished was reported as installed: %v", err)
			}
		})
	}
}

func TestReceiveOSUpdateOnlyForgivesTransportLossAfterInstall(t *testing.T) {
	done := progressFrame("done", 100)
	for _, tc := range []struct {
		name   string
		frames []*agentpb.UpdateOSResponse
		err    error
		lost   bool
		text   string
	}{
		{name: "unavailable after done", frames: []*agentpb.UpdateOSResponse{done}, err: status.Error(codes.Unavailable, "read: connection timed out"), lost: true},
		{name: "unavailable after finalizing", frames: []*agentpb.UpdateOSResponse{progressFrame("finalizing", 100)}, err: status.Error(codes.Unavailable, "read: connection reset by peer"), lost: true},
		{name: "user cancel after done", frames: []*agentpb.UpdateOSResponse{done}, err: status.Error(codes.Canceled, "context canceled"), text: "context canceled"},
		{name: "deadline after done", frames: []*agentpb.UpdateOSResponse{done}, err: status.Error(codes.DeadlineExceeded, "deadline"), text: "deadline"},
		{name: "internal after done", frames: []*agentpb.UpdateOSResponse{done}, err: status.Error(codes.Internal, "stream reset"), text: "stream reset"},
		{name: "unavailable before any progress", err: status.Error(codes.Unavailable, "connection refused"), text: "connection refused"},
		{name: "unavailable after swap", frames: []*agentpb.UpdateOSResponse{progressFrame("swap", -1)}, err: status.Error(codes.Unavailable, "gone"), text: "gone"},
		{name: "EOF after done", frames: []*agentpb.UpdateOSResponse{done}, err: io.EOF, text: "without completion"},
		{name: "failure after done", frames: []*agentpb.UpdateOSResponse{done, {ResponseType: &agentpb.UpdateOSResponse_Failed_{
			Failed: &agentpb.UpdateOSResponse_Failed{ErrorMessage: "marker write failed"},
		}}}, err: io.EOF, text: "marker write failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var phases []string
			err := receiveOSUpdate(&osUpdateTestStream{frames: tc.frames, err: tc.err}, func(phase string, _ int32) {
				phases = append(phases, phase)
			})
			if len(phases) != countProgress(tc.frames) {
				t.Fatalf("progress callbacks = %v", phases)
			}
			if tc.lost {
				if !errors.Is(err, errOSUpdateLostAfterInstall) || !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want lost-after-install wrapping %v", err, tc.err)
				}
				return
			}
			if errors.Is(err, errOSUpdateLostAfterInstall) || err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error = %v, want one containing %q", err, tc.text)
			}
		})
	}
}

func countProgress(frames []*agentpb.UpdateOSResponse) int {
	n := 0
	for _, f := range frames {
		if f.GetProgress() != nil {
			n++
		}
	}
	return n
}

func TestSpinOSUpdateStreamOutcomes(t *testing.T) {
	transportLoss := status.Error(codes.Unavailable, "read: connection timed out")
	for _, tc := range []struct {
		name   string
		frames []*agentpb.UpdateOSResponse
		err    error
		check  func(error) bool
	}{
		{name: "completed", frames: []*agentpb.UpdateOSResponse{progressFrame("done", 100), completedFrame()}, err: io.EOF,
			check: func(err error) bool { return err == nil }},
		{name: "lost after install", frames: []*agentpb.UpdateOSResponse{progressFrame("done", 100)}, err: transportLoss,
			check: func(err error) bool { return errors.Is(err, errOSUpdateLostAfterInstall) }},
		{name: "lost during write", frames: []*agentpb.UpdateOSResponse{progressFrame("write", 60)}, err: transportLoss,
			check: func(err error) bool {
				return errors.Is(err, transportLoss) && !errors.Is(err, errOSUpdateLostAfterInstall)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := osUpdateProgressProgram
			osUpdateProgressProgram = func(model tea.Model) *tea.Program {
				return tui.NewProgressProgram(model, tea.WithInput(nil), tea.WithOutput(io.Discard))
			}
			t.Cleanup(func() { osUpdateProgressProgram = prev })
			got := make(chan error, 1)
			go func() { got <- spinOSUpdateStream(&osUpdateTestStream{frames: tc.frames, err: tc.err}) }()
			select {
			case err := <-got:
				if !tc.check(err) {
					t.Fatalf("spinOSUpdateStream = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("spinner never finished")
			}
		})
	}
}
