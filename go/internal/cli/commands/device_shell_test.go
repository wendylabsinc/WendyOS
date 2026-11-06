package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDeviceShellPlaintextExplainsMTLS(t *testing.T) {
	startUDSAgent(t) // Local plaintext agent; host shell is not registered.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := newDeviceShellCmd()
	cmd.SetContext(ctx)

	err := runDeviceShell(cmd, []string{"true"})
	if err == nil {
		t.Fatal("plain connection unexpectedly opened host shell")
	}
	for _, want := range []string{"authenticated mTLS", "wendy device enroll"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "updat") {
		t.Errorf("plain connection suggests an agent update: %v", err)
	}
}

func TestDeviceShellMacExplainsPlatform(t *testing.T) {
	startUDSAgentWithFeatures(t, "darwin", []string{"native-process"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := newDeviceShellCmd()
	cmd.SetContext(ctx)
	err := runDeviceShell(cmd, []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "not supported by Wendy Agent for macOS") {
		t.Fatalf("error = %v, want macOS support explanation", err)
	}
	if strings.Contains(err.Error(), "wendy device enroll") {
		t.Fatalf("error = %v, enrollment does not enable shell on macOS", err)
	}
}

func TestDeviceShellGoAgentOnDarwinMayOpenShell(t *testing.T) {
	startUDSAgentWithOS(t, "darwin")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.IsMTLS = true // The Go agent serves shell on its authenticated listener.
	if err := hostShellPreflight(ctx, conn); err != nil {
		t.Fatalf("authenticated Go agent on Darwin was refused: %v", err)
	}
}

func TestDeviceShellCancellationDoesNotSuggestEnrollment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := hostShellPreflight(ctx, &grpcclient.AgentConnection{})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancelled shell preflight = %v, want quiet user cancellation", err)
	}
}

func TestShellNeedsTTY(t *testing.T) {
	if !shellNeedsTTY(nil) {
		t.Fatal("nil command (bare login shell) should need a TTY")
	}
	if shellNeedsTTY([]string{"ls"}) {
		t.Fatal("explicit command should not need a TTY")
	}
}

func TestBuildShellStart_CommandAndSize(t *testing.T) {
	// Explicit command is forwarded verbatim.
	req := buildShellStart([]string{"ls", "-la"}, 24, 80)
	st := req.GetStart()
	if st == nil {
		t.Fatal("expected Start")
	}
	if got := st.GetCommand(); len(got) != 2 || got[0] != "ls" || got[1] != "-la" {
		t.Fatalf("command = %v, want [ls -la]", got)
	}
	if st.GetTermSize().GetRows() != 24 || st.GetTermSize().GetCols() != 80 {
		t.Fatalf("term size = %v", st.GetTermSize())
	}

	// Empty command -> empty Command (agent resolves the login shell).
	req2 := buildShellStart(nil, 24, 80)
	if got := req2.GetStart().GetCommand(); len(got) != 0 {
		t.Fatalf("command = %v, want empty (login shell)", got)
	}

	// 24x80 default propagates through.
	req3 := buildShellStart([]string{"sh"}, 24, 80)
	if req3.GetStart().GetTermSize().GetRows() != 24 || req3.GetStart().GetTermSize().GetCols() != 80 {
		t.Fatalf("term size = %v, want 24x80", req3.GetStart().GetTermSize())
	}
}

func TestReceiveHostShellRequiresExitCode(t *testing.T) {
	output := func(data string) *agentpb.HostShellResponse {
		return &agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: []byte(data)}}
	}
	exit := func(code int32) *agentpb.HostShellResponse {
		return &agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: code}}
	}
	transportErr := status.Error(codes.Unavailable, "connection lost")
	for _, tc := range []struct {
		name         string
		frames       []*agentpb.HostShellResponse
		terminal     error
		wantOutput   string
		wantError    error
		wantContains string
	}{
		{name: "empty EOF", terminal: io.EOF, wantError: io.ErrUnexpectedEOF},
		{name: "complete-looking output then EOF", frames: []*agentpb.HostShellResponse{output("UPDATE_END\n")}, terminal: io.EOF, wantOutput: "UPDATE_END\n", wantError: io.ErrUnexpectedEOF},
		{name: "empty response is not zero exit", frames: []*agentpb.HostShellResponse{{}}, terminal: io.EOF, wantError: io.ErrUnexpectedEOF},
		{name: "explicit zero exit", frames: []*agentpb.HostShellResponse{exit(0)}},
		{name: "output and zero exit", frames: []*agentpb.HostShellResponse{output("one\r\n"), output("two"), exit(0)}, wantOutput: "one\r\ntwo"},
		{name: "nonzero exit", frames: []*agentpb.HostShellResponse{output("failed"), exit(69)}, wantOutput: "failed", wantContains: "remote shell exited with code 69"},
		{name: "transport error", frames: []*agentpb.HostShellResponse{output("partial")}, terminal: transportErr, wantOutput: "partial", wantError: transportErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bytes.Buffer
			index := 0
			recv := func() (*agentpb.HostShellResponse, error) {
				if index < len(tc.frames) {
					frame := tc.frames[index]
					index++
					return frame, nil
				}
				if tc.terminal == nil {
					t.Fatal("read past final exit code")
				}
				return nil, tc.terminal
			}
			err := receiveHostShell(recv, &got)
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("error = %v; want %v", err, tc.wantError)
				}
			} else if tc.wantContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantContains) {
					t.Fatalf("error = %v; want %q", err, tc.wantContains)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.wantOutput {
				t.Fatalf("output = %q; want %q", got.String(), tc.wantOutput)
			}
		})
	}
}

func TestReceiveHostShellOutputFailure(t *testing.T) {
	writeErr := errors.New("output closed")
	err := receiveHostShell(func() (*agentpb.HostShellResponse, error) {
		return &agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: []byte("data")}}, nil
	}, failingShellWriter{writeErr})
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v; want output failure", err)
	}
}

type failingShellWriter struct{ err error }

func (w failingShellWriter) Write([]byte) (int, error) { return 0, w.err }
