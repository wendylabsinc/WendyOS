package jobops_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type host struct {
	agentpb.UnimplementedWendyShellServiceServer
	calls              atomic.Int32
	noExit             bool
	corrupt            bool
	failInspect        bool
	inspectCalls       atomic.Int32
	inspectFailures    int32
	inspectFailureCode codes.Code
	lastWait           atomic.Int32
	env                []string
}

func (h *host) HostShell(stream grpc.BidiStreamingServer[agentpb.HostShellRequest, agentpb.HostShellResponse]) error {
	h.calls.Add(1)
	r, err := stream.Recv()
	if err != nil {
		return err
	}
	args := r.GetStart().GetCommand()
	if len(args) != 4 || args[0] != "python3" || args[1] != "-c" {
		return stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: 99}})
	}
	if strings.Contains(args[3], `"until_terminal"`) {
		if n := h.inspectCalls.Add(1); n <= h.inspectFailures {
			return status.Error(h.inspectFailureCode, "injected observation transport failure")
		}
	}
	var payload map[string]any
	if json.Unmarshal([]byte(args[3]), &payload) == nil {
		if wait, ok := payload["wait_seconds"].(float64); ok {
			h.lastWait.Store(int32(wait))
		}
	}
	if h.failInspect && strings.Contains(args[3], `"until_terminal"`) {
		return stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: 3}})
	}
	// Exercise the real embedded read-only program through the public gRPC seam.
	cmd := exec.CommandContext(stream.Context(), args[0], args[1:]...)
	if len(h.env) > 0 {
		cmd.Env = append(os.Environ(), h.env...)
	}
	data, runErr := cmd.CombinedOutput()
	code := int32(0)
	if runErr != nil {
		code = 1
	}
	if h.corrupt {
		data = []byte(strings.Replace(string(data), `"sha256":"`, `"sha256":"0000`, 1))
	}
	if err = stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_StdoutData{StdoutData: data}}); err != nil {
		return err
	}
	if h.noExit {
		return nil
	}
	return stream.Send(&agentpb.HostShellResponse{ResponseType: &agentpb.HostShellResponse_ExitCode{ExitCode: code}})
}
func connection(t *testing.T, h *host) *grpcclient.AgentConnection {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local adapter executes POSIX host probes; Windows clients still use the remote Linux transport")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyShellServiceServer(srv, h)
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	c := grpcclient.NewFromConn(cc)
	c.IsMTLS = true
	return c
}
func fixture(t *testing.T) string {
	t.Helper()
	r := t.TempDir()
	if err := os.WriteFile(filepath.Join(r, "status.json"), []byte(`{"state":"running","stage":"train","runner_pid":999999999,"stages":[{"label":"selftest","returncode":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestInspectThroughHostTransportAndCursor(t *testing.T) {
	c := connection(t, &host{})
	root := fixture(t)
	r, err := jobops.Inspect(context.Background(), c, root, jobops.InspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r["reported_state"] != "running" || r["health"] != "not_running" || r["process"].(map[string]any)["status"] != "missing" {
		t.Fatalf("false readiness: %+v", r)
	}
	next, err := jobops.Inspect(context.Background(), c, root, jobops.InspectOptions{Since: r["cursor"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(next)
	if next["changed"] != false || len(encoded) > 360 {
		t.Fatalf("unbounded unchanged response: %s", encoded)
	}
	data, _ := os.ReadFile(filepath.Join(root, "status.json"))
	if strings.Contains(string(data), "cursor") {
		t.Fatal("inspection changed the job")
	}
}
func TestInspectRejectsUnauthenticatedOrInvalidRequestsBeforeHostExecution(t *testing.T) {
	h := &host{}
	c := connection(t, h)
	c.IsMTLS = false
	if _, err := jobops.Inspect(context.Background(), c, fixture(t), jobops.InspectOptions{}); err == nil {
		t.Fatal("unauthenticated host read")
	}
	c.IsMTLS = true
	for _, tc := range []struct {
		path    string
		options jobops.InspectOptions
	}{{"relative", jobops.InspectOptions{}}, {"/", jobops.InspectOptions{}}, {"/tmp/job", jobops.InspectOptions{WaitSeconds: 1}}, {"/tmp/job", jobops.InspectOptions{WaitSeconds: 31}}} {
		if _, err := jobops.Inspect(context.Background(), c, tc.path, tc.options); err == nil {
			t.Fatalf("accepted invalid request %+v", tc)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("invalid request reached host")
	}
}
func TestInspectRequiresFinalExitStatus(t *testing.T) {
	c := connection(t, &host{noExit: true})
	_, err := jobops.Inspect(context.Background(), c, fixture(t), jobops.InspectOptions{})
	if err == nil || !strings.Contains(err.Error(), "without an exit status") {
		t.Fatalf("lost exit status: %v", err)
	}
}
func TestInspectMissingReportFailsInsteadOfReturningHealthy(t *testing.T) {
	c := connection(t, &host{})
	_, err := jobops.Inspect(context.Background(), c, t.TempDir(), jobops.InspectOptions{})
	if err == nil {
		t.Fatal("missing status was accepted")
	}
}

func TestComputeStatusMissingNvidiaToolStaysUnknown(t *testing.T) {
	c := connection(t, &host{env: []string{"PATH=" + t.TempDir()}})
	result, err := jobops.ComputeStatus(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if result["gpu_state"] != "unknown" || result["gpu_process_count"] != nil {
		t.Fatalf("missing tools reported idle: %+v", result)
	}
}
