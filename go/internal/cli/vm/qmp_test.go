package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnsureTCPPortsChecksQEMUAndReusesExistingForward(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%v", conflict), func(t *testing.T) {
			s := shortQMPStore(t)
			ln, err := net.Listen("unix", s.QMPPath("test"))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				e, d := json.NewEncoder(c), json.NewDecoder(c)
				_ = e.Encode(map[string]any{"QMP": map[string]any{}})
				forwarded, additions := false, 0
				for {
					var req struct {
						Execute   string            `json:"execute"`
						Arguments map[string]string `json:"arguments"`
					}
					if err := d.Decode(&req); err != nil {
						if !conflict && additions != 1 {
							done <- fmt.Errorf("added %d times", additions)
						} else {
							done <- nil
						}
						return
					}
					_ = e.Encode(map[string]any{"event": "RESUME"}) // asynchronous events must be skipped
					var result any = map[string]any{}
					if req.Execute == "human-monitor-command" {
						switch req.Arguments["command-line"] {
						case "info usernet":
							result = "Hub -1 (net0):\n"
							if forwarded {
								result = result.(string) + " TCP[HOST_FORWARD] 12 127.0.0.1 18080 10.0.2.15 18080 0 0\n"
							}
						case "hostfwd_add net0 tcp:127.0.0.1:18080-:18080":
							additions++
							result = ""
							if conflict {
								result = "Could not set up host forwarding rule"
							} else {
								forwarded = true
							}
						default:
							done <- fmt.Errorf("unexpected command: %+v", req)
							return
						}
					}
					_ = e.Encode(map[string]any{"return": result})
				}
			}()
			err = s.EnsureTCPPorts(context.Background(), "test", []int{18080, 18080})
			if conflict {
				if err == nil || !strings.Contains(err.Error(), "free the host port") {
					t.Fatalf("want conflict guidance, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func shortQMPStore(t *testing.T) *Store {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("VM control sockets currently supported on macOS and Linux")
	}
	root, err := os.MkdirTemp("/tmp", "qmp-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	s := &Store{Root: root}
	if err := os.Mkdir(s.Dir("test"), 0700); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPrepareQMPEnforcesPrivateDirectory(t *testing.T) {
	s := shortQMPStore(t)
	if err := os.Chmod(s.Dir("test"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareQMP("test"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Dir("test"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("socket directory permissions = %o; want 0700", info.Mode().Perm())
	}
	if err := os.Symlink(s.Dir("test"), s.Dir("linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareQMP("linked"); err == nil {
		t.Fatal("accepted a symlink for the control socket directory")
	}
}

func TestHasTCPForwardRequiresExactNetAndEndpoints(t *testing.T) {
	for _, line := range []string{
		"Hub -1 (other):\n TCP[HOST_FORWARD] 12 127.0.0.1 18080 10.0.2.15 18080 0 0",
		"Hub -1 (net0):\n TCP[HOST_FORWARD] 12 0.0.0.0 18080 10.0.2.15 18080 0 0",
		"Hub -1 (net0):\n TCP[HOST_FORWARD] 12 127.0.0.1 18080 10.0.2.15 8080 0 0",
		"Hub -1 (net0):\n TCP[ESTABLISHED] 12 127.0.0.1 18080 10.0.2.15 18080 0 0",
	} {
		if hasTCPForward(line, 18080) {
			t.Errorf("accepted wrong forward: %s", line)
		}
	}
}
