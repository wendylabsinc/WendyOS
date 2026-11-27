//go:build linux

package appservice

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Real Linux sockets and child processes exercise SO_PEERCRED. The explicit
// PID-to-deployment registry is a test commissioning adapter, not a claim that
// production container identity can be inferred from UID or a process header.
func TestLinuxExactServicePeerCheck(t *testing.T) {
	type binding struct {
		service    string
		generation uint64
	}
	var mu sync.Mutex
	bindings := map[int32]binding{}
	f := setup(t, func(cfg *Config) {
		cfg.PeerCheck = func(_ context.Context, peer Peer, scope serviceorigin.Scope) error {
			mu.Lock()
			defer mu.Unlock()
			known, ok := bindings[peer.PID]
			if !ok || known.service != scope.Service || known.generation != scope.DeploymentGeneration {
				return ErrPeer
			}
			return nil
		}
	})
	a := f.mount(t, f.scope)
	bScope := f.scope
	bScope.Service = "detector-b"
	b := f.mount(t, bScope)
	run := func(path, service string, generation uint64) string {
		t.Helper()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestLinuxPublicationPeerProcess$")
		cmd.Env = append(os.Environ(), "WENDY_WORLD_TEST_PEER_SOCKET="+path)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		bindings[int32(cmd.Process.Pid)] = binding{service, generation}
		mu.Unlock()
		_ = input.Close()
		if err = cmd.Wait(); err != nil {
			t.Fatalf("peer helper failed: %v: %s", err, output.String())
		}
		mu.Lock()
		delete(bindings, int32(cmd.Process.Pid))
		mu.Unlock()
		return strings.TrimSpace(output.String())
	}
	if got := run(a.SocketPath, "detector-a", 1); got != "accepted" {
		t.Fatal("exact peer denied", got)
	}
	if got := run(b.SocketPath, "detector-a", 1); got != "denied" {
		t.Fatal("same-app foreign service accepted", got)
	}
	if got := run(a.SocketPath, "detector-a", 2); got != "denied" {
		t.Fatal("wrong deployment accepted", got)
	}
	if f.count() != 1 {
		t.Fatal("incorrectly attributed child publication")
	}
}

func TestLinuxPublicationPeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_WORLD_TEST_PEER_SOCKET")
	if path == "" {
		return
	}
	if os.Getenv("WENDY_WORLD_TEST_PIN_PEER") == "1" {
		conn, err := net.Dial("unix", path)
		if err != nil {
			os.Exit(2)
		}
		if _, err = conn.Write([]byte("buffered-publication")); err != nil {
			os.Exit(3)
		}
		_, _ = io.ReadAll(os.Stdin)
		os.Exit(0)
	}
	_, _ = io.ReadAll(os.Stdin)
	client, _ := dial(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	reply, err := client.Publish(ctx, request())
	if err == nil && reply.Accepted {
		_, _ = os.Stdout.WriteString("accepted\n")
	} else {
		_, _ = os.Stdout.WriteString("denied\n")
	}
	os.Exit(0)
}

func TestLinuxPinnedPeerRejectsBufferedRequestAfterExit(t *testing.T) {
	for _, when := range []string{"before-peer-check", "during-peer-check"} {
		t.Run(when, func(t *testing.T) {
			f := setup(t, nil)
			path := f.config.Directory + "/pinned.sock"
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_ = listener.SetDeadline(time.Now().Add(2 * time.Second))
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestLinuxPublicationPeerProcess$")
			cmd.Env = append(os.Environ(), "WENDY_WORLD_TEST_PEER_SOCKET="+path, "WENDY_WORLD_TEST_PIN_PEER=1")
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			conn, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			pin, err := pinOriginalPeer(conn)
			if err != nil {
				_ = conn.Close()
				t.Fatal("Linux 6.5+ SO_PEERPIDFD required", err)
			}
			t.Log("kernel SO_PEERPIDFD captured the original connecting process")
			s := &service{manager: f.manager, scope: f.scope, ctx: context.Background(), connections: map[*trackedConn]struct{}{}}
			tracked := &trackedConn{UnixConn: conn, service: s, pin: pin}
			defer tracked.Close()
			exitPeer := func() {
				t.Helper()
				_ = input.Close()
				if err := cmd.Wait(); err != nil {
					t.Fatalf("peer exit failed: %v %s", err, output.String())
				}
			}
			f.manager.cfg.PeerCheck = func(context.Context, Peer, serviceorigin.Scope) error { return nil }
			if _, err = s.checkPeer(context.Background(), tracked); err != nil {
				t.Fatal("live original peer denied", err)
			}
			if when == "before-peer-check" {
				exitPeer()
				// Model a reused numeric PID now pointing at an allowed process.
				// The original kernel pidfd remains dead and must win over this seam.
				f.manager.peerCredentials = func(*net.UnixConn) (Peer, error) { return Peer{PID: int32(os.Getpid())}, nil }
			} else {
				f.manager.cfg.PeerCheck = func(context.Context, Peer, serviceorigin.Scope) error { exitPeer(); return nil }
			}
			if _, err = s.checkPeer(context.Background(), tracked); err == nil {
				t.Fatal("dead original peer authorized through a reusable PID")
			}
			buffer := make([]byte, len("buffered-publication"))
			if _, err = io.ReadFull(conn, buffer); err != nil || string(buffer) != "buffered-publication" {
				t.Fatal("test did not retain buffered original-peer data", err)
			}
			if pin.Alive() == nil {
				t.Fatal("dead original process pin stayed live")
			}
		})
	}
}
