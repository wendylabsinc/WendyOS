//go:build linux

package supervisor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

func TestAgentSocketMatchesTheAgent(t *testing.T) {
	if AgentSocket != oci.AdminAgentSocketHostPath {
		t.Fatalf("AgentSocket = %q, the agent listens on %q", AgentSocket, oci.AdminAgentSocketHostPath)
	}
}

type fakeAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	sha string
}

func (f fakeAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "2026.11.02-101010", BinarySha256: f.sha}, nil
}

func serveFakeAgent(t *testing.T, sha string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, fakeAgent{sha: sha})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return sock
}

func TestSocketVersionReportsTheRunningHash(t *testing.T) {
	sock := serveFakeAgent(t, "abc")
	sha, err := socketVersion(context.Background(), sock)
	if err != nil || sha != "abc" {
		t.Fatalf("socketVersion = %q, %v", sha, err)
	}
}

func TestSocketVersionFailsWithoutAnAgent(t *testing.T) {
	if _, err := socketVersion(context.Background(), filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatal("want an error when nothing listens")
	}
}

func TestListensOnFindsTheProcessListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(lis.Addr().String()[strings.LastIndex(lis.Addr().String(), ":"):], ":"))

	if !listensOn(os.Getpid(), port) {
		t.Fatalf("pid %d listens on %d but listensOn says no", os.Getpid(), port)
	}
	if listensOn(os.Getpid(), port+1) {
		t.Fatal("a port nobody listens on must not match")
	}
}

func TestRemoteSeenMatchesTheProcessAndThePath(t *testing.T) {
	dir := t.TempDir()
	if remoteSeen(dir, 42, "relay") {
		t.Fatal("no mark means not seen")
	}
	if err := os.WriteFile(filepath.Join(dir, "remote-seen.relay"), []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !remoteSeen(dir, 42, "relay") {
		t.Fatal("the agent with pid 42 was reached over the relay")
	}
	if remoteSeen(dir, 43, "relay") {
		t.Fatal("a mark left by another process must not count")
	}
	if remoteSeen(dir, 42, "lan") {
		t.Fatal("a relay call must not confirm an update that came over the LAN")
	}
}

func TestEmergencyFromTheKernelCommandLine(t *testing.T) {
	cmdline := filepath.Join(t.TempDir(), "cmdline")
	flag := filepath.Join(t.TempDir(), "flag")
	os.WriteFile(cmdline, []byte("root=/dev/sda2 wendy.agent=image quiet\n"), 0o644)
	if !emergency(flag, cmdline) {
		t.Fatal("wendy.agent=image must force the image agent")
	}
	os.WriteFile(cmdline, []byte("root=/dev/sda2 quiet\n"), 0o644)
	if emergency(flag, cmdline) {
		t.Fatal("no flag means no emergency")
	}
	os.WriteFile(flag, nil, 0o644)
	if !emergency(flag, cmdline) {
		t.Fatal("the flag file must force the image agent")
	}
}

func TestTheSocketReasonIsShort(t *testing.T) {
	dir := t.TempDir()
	refused := filepath.Join(dir, "refused.sock")
	l, err := net.Listen("unix", refused)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	mute := filepath.Join(dir, "mute.sock")
	ml, err := net.Listen("unix", mute)
	if err != nil {
		t.Fatal(err)
	}
	defer ml.Close()
	go func() {
		for {
			c, err := ml.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	for sock, want := range map[string]string{
		filepath.Join(dir, "none.sock"): "agent does not answer on its local socket (no socket)",
		refused:                         "agent does not answer on its local socket (connection refused)",
		mute:                            "agent does not answer on its local socket (timed out)",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := socketVersion(ctx, sock)
		cancel()
		// The reason reaches users; raw gRPC text reads like their own connection failed.
		if got := socketProbeError(err).Error(); got != want {
			t.Errorf("%s: got %q, want %q", filepath.Base(sock), got, want)
		}
	}
}

func TestTrialProbeGivesAShortSocketReason(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	path, sum := agentVariant(t, filepath.Dir(store.BinPath("x")), "probe")
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	probe := TrialProbe(store, filepath.Join(t.TempDir(), "none.sock"), 1)
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		// Until exec completes, /proc shows the parent's executable.
		if err = probe(context.Background(), cmd.Process.Pid, sum); err != nil && !strings.Contains(err.Error(), "not running the new agent") {
			break
		}
	}
	if err == nil || err.Error() != "agent does not answer on its local socket (no socket)" {
		t.Fatalf("got %v", err)
	}
}

func TestUpdatingMatchesTheProcess(t *testing.T) {
	dir := t.TempDir()
	updating := UpdatingFunc(dir)
	if updating(42) {
		t.Fatal("no mark means no OS update")
	}
	os.WriteFile(filepath.Join(dir, UpdateMark), []byte("42"), 0o644)
	if !updating(42) || updating(43) {
		t.Fatal("only the agent that wrote the mark is installing")
	}
}
