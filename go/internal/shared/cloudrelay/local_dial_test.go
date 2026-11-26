package cloudrelay

import (
	"context"
	"net"
	"runtime"
	"strconv"
	"testing"
)

func TestRelaySessionsReachTheAgentFromTheRelayAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux routes all of 127.0.0.0/8 on loopback")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accepted := make(chan net.Addr, 1)
	go func() {
		if c, err := lis.Accept(); err == nil {
			accepted <- c.RemoteAddr()
			c.Close()
		}
	}()

	port := lis.Addr().(*net.TCPAddr).Port
	c, err := dialLocalService(context.Background(), "127.0.0.1", strconv.Itoa(port), true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := (<-accepted).(*net.TCPAddr).IP; !got.Equal(RelaySourceIP) {
		t.Fatalf("the agent saw the relay session from %s, want %s", got, RelaySourceIP)
	}
}

func TestAppTunnelsKeepAPlainLoopbackSource(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accepted := make(chan net.Addr, 1)
	go func() {
		if c, err := lis.Accept(); err == nil {
			accepted <- c.RemoteAddr()
			c.Close()
		}
	}()
	port := lis.Addr().(*net.TCPAddr).Port
	c, err := dialLocalService(context.Background(), "127.0.0.1", strconv.Itoa(port), false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Published app ports hairpin through NAT rules keyed on 127.0.0.1.
	if got := (<-accepted).(*net.TCPAddr).IP; got.Equal(RelaySourceIP) {
		t.Fatalf("an app tunnel must not use %s", RelaySourceIP)
	}
}

func TestEverySessionToTheAgentPortCountsAsAnAgentSession(t *testing.T) {
	a := &Agent{MTLSPort: 50052}
	cases := []struct {
		descriptor string
		port       int
		want       bool
	}{
		{"wendy-agent", 50051, true},
		// An app tunnel aimed at the agent's own listener is still the agent.
		{"tcp-50052", 50052, true},
		{"tcp-8080", 8080, false},
		{"ssh", 22, false},
	}
	for _, c := range cases {
		if got := a.isAgentSession(c.descriptor, c.port); got != c.want {
			t.Errorf("isAgentSession(%q, %d) = %v, want %v", c.descriptor, c.port, got, c.want)
		}
	}
	if (&Agent{}).isAgentSession("wendy-agent", 50051) {
		t.Error("without an mTLS port there is no agent session to tag")
	}
}
