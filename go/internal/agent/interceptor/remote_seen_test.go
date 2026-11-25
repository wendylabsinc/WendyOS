package interceptor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"go.uber.org/zap"
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

func TestClearRemoteSeenRemovesOnlyThisAgentsMark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	mark := filepath.Join(dir, "remote-seen.cloud")
	if err := MarkRemoteSeen(dir, "cloud"); err != nil {
		t.Fatal(err)
	}
	if err := ClearRemoteSeen(dir, "cloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Fatalf("this agent's mark must be gone, stat err = %v", err)
	}
	os.WriteFile(mark, []byte("1"), 0o644)
	if err := ClearRemoteSeen(dir, "cloud"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(mark); string(got) != "1" {
		t.Fatalf("another agent's mark must stay, got %q", got)
	}
}

// On a Closed device's keyless listener, wired as main does (authorizer, then
// remote-seen), neither a refused call nor GetAccess approves an agent on
// trial; a call the keyless set admits does.
func TestClosedRemoteSeenCountsOnlyAdmittedCalls(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := keys.Close(spkiOf(t), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	authz := (&ClosedAuthorizer{Logger: zap.NewNop(), Listener: ClosedKeylessListener, Keys: keys}).Unary()
	seen := UnaryRemoteSeen(dir)
	lan := withPeer(&net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 4000})
	call := func(method string) {
		info := &grpc.UnaryServerInfo{FullMethod: method}
		_, _ = authz(lan, nil, info, func(ctx context.Context, req any) (any, error) {
			return seen(ctx, req, info, func(context.Context, any) (any, error) { return nil, nil })
		})
	}
	marked := func() bool { e, _ := os.ReadDir(dir); return len(e) > 0 }

	call("/wendy.agent.services.v1.WendyContainerService/ListContainers")
	call("/wendy.agent.services.v2.WendyAccessService/GetAccess")
	if marked() {
		t.Fatal("a refused call or GetAccess approved the agent on trial")
	}
	if err := keys.SetKeylessApps(true, owner.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	call("/wendy.agent.services.v1.WendyContainerService/ListContainers")
	if !marked() {
		t.Fatal("a call the keyless set admitted did not count")
	}
}
