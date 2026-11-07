//go:build linux

package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// AgentSocket must match oci.AdminAgentSocketHostPath (checked by a test);
// importing oci would pull containerd into this small binary.
const AgentSocket = "/var/lib/wendy/agent-control/agent.sock"

// socketVersion asks the agent on its local socket which binary it runs.
func socketVersion(ctx context.Context, sock string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	resp, err := agentpb.NewWendyAgentServiceClient(conn).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return "", err
	}
	return resp.GetBinarySha256(), nil
}

// listensOn reports whether pid owns a listening TCP socket on port.
func listensOn(pid, port int) bool {
	want := map[string]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		f, err := os.Open(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" { // 0A = LISTEN
				continue
			}
			_, hexPort, ok := strings.Cut(fields[1], ":")
			if p, err := strconv.ParseInt(hexPort, 16, 32); ok && err == nil && int(p) == port {
				want["socket:["+fields[9]+"]"] = true
			}
		}
		f.Close()
	}
	if len(want) == 0 {
		return false
	}
	fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	for _, fd := range fds {
		if link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil && want[link] {
			return true
		}
	}
	return false
}

// remoteSeen reports whether the agent with this pid recorded a call from a
// remote client over via ("lan" or "relay"). The pid keeps an old mark from counting.
func remoteSeen(dir string, pid int, via string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "remote-seen."+via))
	return err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid)
}

// emergency is the way back to the image agent when every /data agent is bad.
func emergency(flagPath, cmdlinePath string) bool {
	if _, err := os.Stat(flagPath); err == nil {
		return true
	}
	data, _ := os.ReadFile(cmdlinePath)
	for _, arg := range strings.Fields(string(data)) {
		if arg == "wendy.agent=image" {
			return true
		}
	}
	return false
}

// TrialProbe checks that the process, the local socket and a network port all
// belong to the new agent. sock "" skips the socket (WENDY_LOCAL_SOCKET=off).
func TrialProbe(store Store, sock string, port int) func(context.Context, int, string) error {
	return func(ctx context.Context, pid int, sha string) error {
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil || filepath.Clean(exe) != store.BinPath(sha) {
			return fmt.Errorf("process %d is not running the new agent (%s)", pid, exe)
		}
		if sock != "" {
			got, err := socketVersion(ctx, sock)
			if err != nil {
				return socketProbeError(err)
			}
			if got != sha {
				return fmt.Errorf("local socket is served by %s, not the new agent", got)
			}
		}
		if !listensOn(pid, port) && !listensOn(pid, port+1) {
			return fmt.Errorf("agent is not listening on port %d or %d", port, port+1)
		}
		return nil
	}
}

// socketProbeError names why the local socket did not answer in a few words:
// the reason reaches users, and raw gRPC text reads like their own
// connection failed.
func socketProbeError(err error) error {
	cause := status.Code(err).String()
	switch msg := err.Error(); {
	case errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded:
		cause = "timed out"
	case strings.Contains(msg, "no such file or directory"):
		cause = "no socket"
	case strings.Contains(msg, "connection refused"):
		cause = "connection refused"
	}
	return fmt.Errorf("agent does not answer on its local socket (%s)", cause)
}

// ErrNoLiveness: the agent runs without its local socket, so it cannot be pinged.
var ErrNoLiveness = errors.New("liveness is off: the agent has no local socket")

// AliveProbe pings the running agent on its local socket.
func AliveProbe(sock string) func(context.Context) error {
	return func(ctx context.Context) error {
		if sock == "" {
			return ErrNoLiveness
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := socketVersion(ctx, sock)
		return err
	}
}

// ServingProbe reports whether pid listens on port or port+1 (mTLS).
func ServingProbe(port int) func(pid int) bool {
	return func(pid int) bool { return listensOn(pid, port) || listensOn(pid, port+1) }
}

// UpdateMark is where the agent names itself while it applies an update: an
// agent upload, an online update or an OS update.
const UpdateMark = "update-running"

// UpdatingFunc reports whether the agent with pid is applying an update; the
// pid keeps a mark left by a crashed agent from counting.
func UpdatingFunc(dir string) func(pid int) bool {
	return func(pid int) bool {
		data, err := os.ReadFile(filepath.Join(dir, UpdateMark))
		return err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(pid)
	}
}

func RemoteSeenFunc(dir string) func(pid int, via string) bool {
	return func(pid int, via string) bool { return remoteSeen(dir, pid, via) }
}

func EmergencyFunc(flagPath, cmdlinePath string) func() bool {
	return func() bool { return emergency(flagPath, cmdlinePath) }
}
