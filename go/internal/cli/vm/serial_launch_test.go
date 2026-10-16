//go:build darwin || linux

package vm

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSerialLaunchCleanup(t *testing.T) {
	for _, scenario := range []string{"invalid-spec", "start-failure", "state-failure", "normal-exit", "peer-exit", "peer-loss", "disconnect", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			spec := detachableSpec(t, s, "serial")
			if err := s.ConfigureSerial("serial", &SerialConfig{"COM12", 1000000}); err != nil {
				t.Fatal(err)
			}
			peer := fakeSerial(t)
			saved := vmCommandContext
			defer func() { vmCommandContext = saved }()
			var addr string
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			vmCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				match := regexp.MustCompile(`host=127.0.0.1,port=([0-9]+),`).FindStringSubmatch(strings.Join(args, " "))
				if len(match) != 2 {
					t.Fatal("no loopback serial QEMU argument")
				}
				addr = "127.0.0.1:" + match[1]
				conn, err := net.Dial("tcp4", addr)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				switch scenario {
				case "start-failure":
					return exec.CommandContext(ctx, "/nonexistent-wendy-test")
				case "state-failure":
					if err := os.Remove(s.StatePath("serial")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(s.StatePath("serial"), 0700); err != nil {
						t.Fatal(err)
					}
				case "normal-exit":
					return exec.CommandContext(ctx, "true")
				case "peer-exit":
					conn.Close()
					return exec.CommandContext(ctx, "sleep", "0.1")
				case "peer-loss":
					conn.Close()
				case "disconnect":
					peer.Close()
				case "cancel":
					cancel()
				}
				return exec.CommandContext(ctx, "sleep", "30")
			}
			if scenario == "invalid-spec" {
				spec.FirmwareCode = ""
			}
			err := s.RunForeground(ctx, spec, nil, io.Discard, io.Discard)
			if scenario == "normal-exit" || scenario == "peer-exit" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if scenario == "disconnect" && !strings.Contains(err.Error(), "disconnected") {
				t.Fatal(err)
			}
			if held, err := s.runLockHeld("serial"); held || err != nil {
				t.Fatalf("lock leaked: %v %v", held, err)
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			assertSerialPeerClosed(t, peer)
			if addr != "" {
				if c, err := net.DialTimeout("tcp4", addr, time.Second); err == nil {
					c.Close()
					t.Fatal("listener leaked")
				}
			}
		})
	}
}
func TestSerialDetachedRejected(t *testing.T) {
	s := newTestStore(t)
	spec := detachableSpec(t, s, "serial")
	if err := s.ConfigureSerial("serial", &SerialConfig{"COM12", 1000000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartDetached(spec); err == nil || !strings.Contains(err.Error(), "foreground") {
		t.Fatal(err)
	}
}
