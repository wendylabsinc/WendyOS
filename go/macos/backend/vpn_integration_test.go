package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
)

// Explicit opt-in. Exercises the same backend and packet engine linked into
// libwendydesktop.a and libwendyvpn.a, without changing the system VPN.
func TestVPNDeviceIntegration(t *testing.T) {
	selector := os.Getenv("WENDY_TEST_VPN_DEVICE")
	if selector == "" {
		t.Skip("set WENDY_TEST_VPN_DEVICE for a live device check")
	}
	b := New()
	defer b.Shutdown()
	cfg, err := b.StartVPN([]vpn.Route{{Selector: selector, Name: "VPN test"}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := vpn.NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	t.Run("ICMP", func(t *testing.T) {
		for sequence := range 2 {
			payload := []byte("wendy VPN integration echo")
			body, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 42, Seq: sequence, Data: payload}}).Marshal(nil)
			if err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, 20+len(body))
			packet[0], packet[8], packet[9] = 0x45, 64, 1
			binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
			copy(packet[12:16], net.ParseIP(vpn.TunnelAddress).To4())
			copy(packet[16:20], net.ParseIP(cfg.Routes[0].Address).To4())
			binary.BigEndian.PutUint16(packet[10:12], ^checksum.Checksum(packet[:20], 0))
			copy(packet[20:], body)
			started := time.Now()
			engine.Write(packet)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			reply := engine.Read(ctx)
			cancel()
			if len(reply) < 28 {
				t.Fatal("no ICMP response from selected device")
			}
			message, err := icmp.ParseMessage(1, reply[20:])
			if err != nil {
				t.Fatal(err)
			}
			echo, ok := message.Body.(*icmp.Echo)
			if !ok || message.Type != ipv4.ICMPTypeEchoReply || echo.ID != 42 || echo.Seq != sequence || !bytes.Equal(echo.Data, payload) {
				t.Fatalf("wrong echo response: %+v", message)
			}
			t.Logf("device-backed echo seq=%d rtt=%s", sequence, time.Since(started).Round(time.Millisecond))
		}
	})
	if text := os.Getenv("WENDY_TEST_VPN_HTTP_PORT"); text != "" {
		t.Run("HTTP", func(t *testing.T) {
			port, err := strconv.ParseUint(text, 10, 16)
			if err != nil || port == 0 {
				t.Fatal("invalid HTTP test port")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, err := vpn.DialRelay(ctx, cfg, cfg.Routes[0].Address, uint16(port))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(cfg.Routes[0].Host, text)+"/", nil)
			if err := req.Write(conn); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			t.Logf("HTTP response through VPN relay: %s", response.Status)
		})
	}
}
