package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
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
