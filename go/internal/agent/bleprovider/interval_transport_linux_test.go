//go:build linux && regression_ble_interval

package bleprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Only the syscall boundary is replaced by the required source overlay. The
// production request builder, response parser, tune gate and complete outbound
// provider/TLS/stream path remain unchanged. No Bluetooth socket is opened.
var intervalTransportController *intervalController

type intervalController struct {
	mode             string
	entered, release chan struct{}
	enterOnce        sync.Once
	sent, closed     atomic.Int32
	raw              net.Conn
}

func intervalTransportHandle(net.Conn) (uint16, error) { return 25, nil }

func intervalTransportPrepare(ctx context.Context, index int) (*leUpdateIO, error) {
	c := intervalTransportController
	if c == nil || index != 0 {
		return nil, errors.New("missing owned controller fixture")
	}
	packets := [][]byte{leConnectionUpdateCommand(25, meshIntervalUnits), statusPacket(0)}
	switch c.mode {
	case "command-disallowed":
		packets[1] = statusPacket(0x0c)
	case "unsupported":
		packets[1] = statusPacket(0x01)
	case "completion-refused":
		packets = append(packets, completePacket(25, 36, 0, meshTimeoutUnits, 0x1e))
	case "success":
		packets = append(packets, completePacket(25, 36, 0, meshTimeoutUnits, 0))
	}
	next := 0
	return &leUpdateIO{
		send: func(command []byte) (int, error) {
			c.sent.Add(1)
			c.enterOnce.Do(func() { close(c.entered) })
			if !bytes.Equal(command, leConnectionUpdateCommand(25, meshIntervalUnits)) {
				return 0, errors.New("wrong actual controller request")
			}
			return len(command), nil
		},
		read: func(ctx context.Context) ([]byte, error) {
			select {
			case <-c.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if c.mode == "disconnect" {
				_ = c.raw.Close()
				return nil, io.EOF
			}
			if c.mode == "monitor-lost" {
				return nil, unix.EIO
			}
			if next < len(packets) {
				packet := packets[next]
				next++
				return packet, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		close: func() { c.closed.Add(1) },
	}, nil
}

type intervalTransportConn struct {
	net.Conn
	written atomic.Int64
	closed  atomic.Bool
}

func (c *intervalTransportConn) Write(p []byte) (int, error) {
	c.written.Add(int64(len(p)))
	return c.Conn.Write(p)
}
func (c *intervalTransportConn) Close() error { c.closed.Store(true); return c.Conn.Close() }

type intervalTrafficNode struct {
	self, peer meshid.Device
	result     chan error
	calls      atomic.Int32
}

func (n *intervalTrafficNode) AttachStream(ctx context.Context, peer meshid.Device, conn net.Conn, cost uint16) error {
	n.calls.Add(1)
	err := func() error {
		if peer != n.peer || cost != LinkCost {
			return errors.New("wrong authenticated peer or BLE cost")
		}
		if err := localmesh.OpenStreamHello(ctx, conn, n.self, n.peer); err != nil {
			return err
		}
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		for _, payload := range []string{"interval-link-first", "interval-link-still-alive"} {
			if _, err := io.WriteString(conn, payload); err != nil {
				return err
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				return err
			}
			if string(got) != payload {
				return fmt.Errorf("echo changed: %q", got)
			}
		}
		return nil
	}()
	n.result <- err
	return err
}

func TestBLEControllerOutcomePreservesAuthenticatedTraffic(t *testing.T) {
	if os.Getenv("WENDY_BLE_INTERVAL_POLICY") == "" {
		t.Fatal("required private policy overlay missing")
	}
	// The overlaid btradio policy path has no preexisting saved profile. Default
	// normal admission therefore never submits a separate PHY-control command.
	if _, err := os.Stat(os.Getenv("WENDY_BLE_INTERVAL_POLICY")); !os.IsNotExist(err) {
		t.Fatal("policy fixture must start absent")
	}
	if filepath.Base(os.Getenv("WENDY_BLE_INTERVAL_POLICY")) != "radio.json" {
		t.Fatal("wrong private policy file")
	}
	high, low := issuerCredentials(t, true, ecdsaDeviceKey)
	for _, mode := range []string{"command-disallowed", "unsupported", "completion-refused", "success", "accepted-timeout", "monitor-lost", "disconnect", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			allow := mode == "command-disallowed" || mode == "unsupported" || mode == "completion-refused" || mode == "success"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			a, b := tcpConnectionPair(t)
			raw := &intervalTransportConn{Conn: a}
			controller := &intervalController{mode: mode, entered: make(chan struct{}), release: make(chan struct{}), raw: raw}
			intervalTransportController = controller
			node := &intervalTrafficNode{self: high.Device, peer: low.Device, result: make(chan error, 1)}
			r := &runtime{cfg: Config{Credentials: high, Node: node, NetworkName: "interval-fixture", Logger: zap.NewNop()}, dialSlot: make(chan struct{}, 1), dialCoC: func(context.Context, string, string, uint16) (net.Conn, error) { return raw, nil }}
			peer := issuerPeer(low)
			serverDone := make(chan error, 1)
			go func() {
				secure := tls.Server(b, (&runtime{cfg: Config{Credentials: low}}).makeServerTLS())
				defer secure.Close()
				err := secure.HandshakeContext(ctx)
				if err == nil {
					err = localmesh.OpenStreamHello(ctx, secure, low.Device, high.Device)
				}
				if err == nil {
					_, err = io.Copy(secure, secure)
				}
				serverDone <- err
			}()
			dialDone := make(chan struct{})
			go func() { r.dialLink(ctx, peer); close(dialDone) }()
			t.Cleanup(func() {
				cancel()
				_ = raw.Close()
				_ = b.Close()
				select {
				case <-dialDone:
				case <-time.After(2 * time.Second):
					t.Error("provider did not drain")
				}
				select {
				case <-serverDone:
				case <-time.After(2 * time.Second):
					t.Error("TLS server did not drain")
				}
				intervalTransportController = nil
			})
			select {
			case <-controller.entered:
			case <-time.After(time.Second):
				t.Fatal("actual provider did not submit interval command")
			}
			time.Sleep(20 * time.Millisecond)
			if raw.written.Load() != 0 || node.calls.Load() != 0 {
				t.Fatal("TLS escaped pending controller barrier")
			}
			if mode == "cancelled" {
				cancel()
			}
			start := time.Now()
			close(controller.release)
			select {
			case <-dialDone:
			case <-ctx.Done():
				t.Fatal("provider exceeded bounded controller/traffic lifecycle")
			}
			if allow {
				select {
				case err := <-node.result:
					if err != nil {
						t.Fatalf("controller outcome did not preserve authenticated traffic: %v", err)
					}
				default:
					t.Fatal("controller outcome did not preserve authenticated traffic: no authenticated attachment")
				}
				if controller.sent.Load() != 2 || controller.closed.Load() != 2 || node.calls.Load() != 1 || raw.written.Load() == 0 {
					t.Fatal("initial/steady controller or TLS lifecycle missing")
				}
			} else {
				if node.calls.Load() != 0 || raw.written.Load() != 0 || controller.sent.Load() != 1 || controller.closed.Load() != 1 {
					t.Fatal("uncertain controller outcome sent TLS or leaked controller IO")
				}
				if mode == "accepted-timeout" && time.Since(start) < meshInitialTuneBudget-100*time.Millisecond {
					t.Fatal("accepted command did not retain real controller timeout")
				}
			}
			if !raw.closed.Load() || len(r.dialSlot) != 0 {
				t.Fatal("provider retained owned connection or setup slot")
			}
			t.Logf("BLE_INTERVAL_TRANSPORT_VERIFIED mode=%s authenticated=%v", mode, allow)
		})
	}
}
