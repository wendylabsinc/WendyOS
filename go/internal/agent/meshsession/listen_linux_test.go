//go:build linux

package meshsession

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"
)

func TestAppListenerBeforeVIPAndAddressRecreation(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged Linux container")
	}
	_, credentials := fixtureCredentials(t)
	pc, err := listenAppPackets(context.Background(), credentials, ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		t.Fatal("lost native UDP optimizations")
	}
	endpoint := udp.LocalAddr().(*net.UDPAddr)
	if endpoint.IP.String() != "10.88.1.204" {
		t.Fatalf("not own IPv4 VIP: %s", endpoint)
	}
	raw, _ := udp.SyscallConn()
	checkOption := func(option, want int) {
		t.Helper()
		var got int
		var sockErr error
		err := raw.Control(func(fd uintptr) { got, sockErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, option) })
		if err != nil || sockErr != nil || got != want {
			t.Fatalf("option %d = %d want%d: %v/%v", option, got, want, err, sockErr)
		}
	}
	checkOption(unix.IP_FREEBIND, 1)
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v %v %s", args, err, out)
		}
	}
	var previous int
	for generation := 0; generation < 2; generation++ {
		run("link", "add", "appvip", "type", "dummy")
		defer exec.Command("ip", "link", "del", "appvip").Run()
		run("addr", "add", "10.88.1.204/32", "dev", "appvip")
		run("link", "set", "appvip", "up")
		run("link", "set", "lo", "up")
		iface, _ := net.InterfaceByName("appvip")
		if iface.Index == previous {
			t.Fatal("interface not recreated")
		}
		previous = iface.Index
		client, err := net.DialUDP("udp4", nil, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		client.SetDeadline(time.Now().Add(time.Second))
		udp.SetDeadline(time.Now().Add(time.Second))
		payload := []byte(fmt.Sprint("generation", generation))
		if _, err = client.Write(payload); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, peer, err := udp.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = udp.WriteToUDP(buf[:n], peer); err != nil {
			t.Fatal(err)
		}
		n, err = client.Read(buf)
		client.Close()
		if err != nil || string(buf[:n]) != string(payload) {
			t.Fatalf("recreated VIP echo: %q %v", buf[:n], err)
		}
		run("link", "del", "appvip")
	}
	udp.SetDeadline(time.Time{})
	server, _ := NewServer(credentials, allowPort(func(uint16) bool { return false }))
	listener, err := quic.Listen(pc, server.TLSConfig(), quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	checkOption(unix.IP_PKTINFO, 0) // exact-bound native UDP must not cache an ingress device
	checkOption(unix.IP_RECVTOS, 1) // native ECN setup remains enabled
	listener.Close()
}

func TestAppServerCloseBeforeAndDuringRunReleasesSocket(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint("before=", before), func(t *testing.T) {
			_, credentials := fixtureCredentials(t)
			reserved, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := reserved.LocalAddr().String()
			reserved.Close()
			server, _ := NewServer(credentials, allowPort(func(uint16) bool { return false }))
			if before {
				server.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.Run(ctx, addr) }()
			if !before {
				for server.Addr() == nil {
					select {
					case err := <-done:
						t.Fatalf("early return %v", err)
					case <-ctx.Done():
						t.Fatal("listener unavailable")
					default:
						time.Sleep(time.Millisecond)
					}
				}
				server.Close()
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("Run did not exit after Close")
			}
			rebound, err := net.ListenPacket("udp4", addr)
			if err != nil {
				t.Fatalf("socket leaked after Close: %v", err)
			}
			rebound.Close()
		})
	}
}
