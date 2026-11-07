package interceptor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

func withPeer(addr net.Addr) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
}

func TestRemoteVia(t *testing.T) {
	cases := map[string]struct {
		addr net.Addr
		want string
	}{
		"LAN client":              {&net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 4000}, "lan"},
		"cloud relay":             {&net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 4000}, "relay"},
		"cloud relay, dual stack": {&net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.2"), Port: 4000}, "relay"},
		"other loopback caller":   {&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4000}, ""},
		"IPv6 loopback caller":    {&net.TCPAddr{IP: net.ParseIP("::1"), Port: 4000}, ""},
		"local socket":            {&net.UnixAddr{Name: "@", Net: "unix"}, ""},
		"IPv6 LAN client":         {&net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 4000}, "lan"},
	}
	for name, c := range cases {
		if got := RemoteVia(withPeer(c.addr)); got != c.want {
			t.Errorf("%s: RemoteVia = %q, want %q", name, got, c.want)
		}
	}
	if got := RemoteVia(context.Background()); got != "" {
		t.Errorf("no peer: RemoteVia = %q", got)
	}
}

func TestUnaryRemoteSeenMarksTheTransport(t *testing.T) {
	dir := t.TempDir()
	call := UnaryRemoteSeen(dir)
	handler := func(context.Context, any) (any, error) { return nil, nil }

	if _, err := call(withPeer(&net.UnixAddr{Name: "@", Net: "unix"}), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("a local call must not count as a remote client")
	}

	if _, err := call(withPeer(&net.TCPAddr{IP: net.ParseIP("127.0.0.2")}), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "remote-seen.relay"))
	if err != nil || string(got) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("want this pid in remote-seen.relay, got %q, %v", got, err)
	}
}

func TestMarkRemoteSeenNamesThisAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := MarkRemoteSeen(dir, "cloud"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "remote-seen.cloud"))
	if err != nil || string(got) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the mark must carry this agent's pid, got %q, %v", got, err)
	}
}
