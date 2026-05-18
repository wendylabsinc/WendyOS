package main

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// TestDualStackListenerAcceptsIPv4AndIPv6 verifies that the dual-stack loopback
// listener accepts connections from both 127.0.0.1 (IPv4) and [::1] (IPv6).
// Skipped when IPv6 loopback is not available on the host.
func TestDualStackListenerAcceptsIPv4AndIPv6(t *testing.T) {
	t.Parallel()

	// Grab two listeners on the same port to avoid a TOCTOU port-reuse race.
	lis4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis4.Addr().(*net.TCPAddr).Port

	lis6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		lis4.Close()
		t.Skip("IPv6 loopback not available on this host")
	}

	dsLis := newDualStackListener(lis4, lis6)
	t.Cleanup(func() { dsLis.Close() })

	accepted := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := dsLis.Accept()
			if err != nil {
				return
			}
			conn.Close()
			accepted <- struct{}{}
		}
	}()

	for _, addr := range []string{
		fmt.Sprintf("127.0.0.1:%d", port),
		fmt.Sprintf("[::1]:%d", port),
	} {
		c, dialErr := net.DialTimeout("tcp", addr, time.Second)
		if dialErr != nil {
			t.Errorf("dial %s: %v", addr, dialErr)
			continue
		}
		c.Close()
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-accepted:
		case <-deadline:
			t.Fatalf("only %d/2 connections accepted before timeout", i)
		}
	}
}

// TestListenDualStackLoopback verifies that listenDualStackLoopback returns a
// working listener and that the external IP cannot reach it.
func TestListenDualStackLoopbackLocalhostOnly(t *testing.T) {
	t.Parallel()

	var externalIP string
	ifaces, _ := net.InterfaceAddrs()
	for _, a := range ifaces {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		externalIP = ipNet.IP.String()
		break
	}
	if externalIP == "" {
		t.Skip("no non-loopback IPv4 interface — cannot verify localhost-only property")
	}

	lis, err := listenDualStackLoopback("0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	port := lis.Addr().(*net.TCPAddr).Port

	// IPv4 loopback must connect.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("IPv4 loopback connection refused: %v", err)
	}
	c.Close()

	// External IP must not connect.
	c2, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", externalIP, port), 500*time.Millisecond)
	if err == nil {
		c2.Close()
		t.Errorf("connection via external IP %s:%d succeeded — listener should be loopback-only", externalIP, port)
	}
}
