//go:build linux

package services

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Exercise ConnectSystemBus itself: a stubbed connectivity function would miss
// a connection stuck before the first NetworkManager property call (#2661).
func TestUplinkProbeCancelsBlockedBusSetup(t *testing.T) {
	for _, phase := range []string{"authentication", "hello"} {
		for _, callerCancels := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/caller_cancel=%v", phase, callerCancels), func(t *testing.T) {
				listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", fmt.Sprintf("tcp:host=127.0.0.1,port=%d", listener.Addr().(*net.TCPAddr).Port))
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				ifaces := []*agentpb.NetworkInterface{{Name: "fixture-uplink", IpAddresses: []string{"192.0.2.10"}}}
				done := make(chan struct{})
				started := time.Now()
				go func() {
					addUplinkInfo(ctx, ifaces)
					close(done)
				}()
				// Closing the fixture on failure also releases an implementation
				// that ignores cancellation; do not leave its goroutine behind.
				var peer *net.TCPConn
				t.Cleanup(func() {
					cancel()
					if peer != nil {
						_ = peer.Close()
					}
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("uplink probe did not stop after fixture cleanup")
					}
				})
				peer, err = listener.AcceptTCP()
				if err != nil {
					t.Fatalf("uplink probe did not connect to the fixture: %v", err)
				}
				if err := peer.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(peer)
				readLine := func(want string) {
					t.Helper()
					line, err := reader.ReadString('\n')
					if err != nil || line != want {
						t.Fatalf("bus handshake: got %q, %v; want %q", line, err, want)
					}
				}
				readLine("\x00AUTH\r\n")
				if phase == "hello" {
					if _, err := io.WriteString(peer, "REJECTED EXTERNAL\r\n"); err != nil {
						t.Fatal(err)
					}
					readLine("AUTH EXTERNAL\r\n")
					if _, err := io.WriteString(peer, "OK 0123456789abcdef0123456789abcdef\r\n"); err != nil {
						t.Fatal(err)
					}
					readLine("BEGIN\r\n")
					msg, err := dbus.DecodeMessage(reader)
					if err != nil {
						t.Fatalf("read initial bus call: %v", err)
					}
					if msg.Type != dbus.TypeMethodCall || msg.Headers[dbus.FieldMember].Value() != "Hello" || msg.Headers[dbus.FieldDestination].Value() != "org.freedesktop.DBus" {
						t.Fatalf("expected the initial bus Hello, got %v", msg)
					}
				}
				select {
				case <-done:
					t.Fatal("probe returned before its blocked setup was cancelled")
				default:
				}
				wait := uplinkProbeTimeout + time.Second
				if callerCancels {
					cancel()
					wait = time.Second
				}
				select {
				case <-done:
				case <-time.After(wait):
					t.Fatal("blocked D-Bus setup outlived the probe's cancellation deadline")
				}
				if !callerCancels && time.Since(started) < uplinkProbeTimeout {
					t.Fatal("probe returned without waiting for its own timeout")
				}
				if got := ifaces[0].GetConnectivity(); got != agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_UNKNOWN {
					t.Fatalf("failed setup invented connectivity: %v", got)
				}
				// Returning is insufficient if an abandoned D-Bus setup goroutine
				// still owns the socket. The peer must observe the client closing it.
				if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if b, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
					t.Fatalf("bus connection was not closed: byte=%#x err=%v", b, err)
				}
			})
		}
	}
}
