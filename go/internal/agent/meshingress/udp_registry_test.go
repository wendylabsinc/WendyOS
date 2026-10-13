package meshingress

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestUDPClaimsAreProtocolAndIncarnationScoped(t *testing.T) {
	r := NewRegistry()
	if err := r.ClaimForApp("tcp", "com.wendy.tcp", 7777); err != nil {
		t.Fatal(err)
	}
	if r.AllowedUDP(7777) {
		t.Fatal("TCP claim authorized UDP")
	}
	if err := r.ClaimUDPForApp("app-a", "com.wendy.a", 7777); err != nil {
		t.Fatal(err)
	}
	a := r.UDPToken(7777)
	if a == 0 || !r.AllowedUDPApp("com.wendy.a", 7777) {
		t.Fatal("missing UDP claim")
	}
	ip := netip.MustParseAddr("10.79.42.2")
	dst := netip.MustParseAddr("10.99.2.22")
	if err := r.ClaimSource("app-a", "com.wendy.a", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	if got := r.SourceIP("app-a"); got != ip.String() {
		t.Fatalf("stop lost current app source: %q", got)
	}
	sourceA := r.SourceToken(ip, dst, 7)
	if sourceA == 0 || r.SourceToken(ip, dst, 8) != 0 || r.SourceToken(ip, netip.MustParseAddr("10.98.2.22"), 7) != 0 {
		t.Fatal("source claim ignored interface or CIDR")
	}
	r.Release("app-a")
	if got := r.SourceIP("app-a"); got != "" {
		t.Fatalf("stopped app retained source: %q", got)
	}
	if err := r.ClaimUDPForApp("app-b", "com.wendy.b", 7777); err != nil {
		t.Fatal(err)
	}
	if err := r.ClaimSource("app-b", "com.wendy.b", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	if r.UDPToken(7777) == a || r.SourceToken(ip, dst, 7) == sourceA {
		t.Fatal("replacement app inherited old flow token")
	}
	called := false
	if err := r.WithAuthorizedUDPToken(7777, a, func() error { called = true; return nil }); !errors.Is(err, ErrPortDenied) || called {
		t.Fatal("old app UDP flow entered replacement app")
	}
	if err := r.WithAuthorizedUDPToken(7777, r.UDPToken(7777), func() error { called = true; return nil }); err != nil || !called {
		t.Fatal("replacement app was not authorized")
	}
}

func TestReleaseWaitsForInFlightUDPReplyAndRejectsReusedSource(t *testing.T) {
	r := NewRegistry()
	ip, dst := netip.MustParseAddr("10.79.42.2"), netip.MustParseAddr("10.99.2.22")
	if err := r.ClaimSource("app-a", "com.wendy.a", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	token := r.SourceToken(ip, dst, 7)
	entered, releaseSend, sendDone, revokeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		sendDone <- r.WithSourceToken(ip, dst, 7, token, func() error { close(entered); <-releaseSend; return nil })
	}()
	<-entered
	go func() { r.Release("app-a"); close(revokeDone) }()
	select {
	case <-revokeDone:
		t.Fatal("revocation raced in-flight reply")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSend)
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-revokeDone:
	case <-time.After(time.Second):
		t.Fatal("revocation did not finish")
	}
	if err := r.ClaimSource("app-b", "com.wendy.b", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := r.WithSourceToken(ip, dst, 7, token, func() error { called = true; return nil }); !errors.Is(err, ErrPortDenied) || called {
		t.Fatal("old UDP reply entered replacement app")
	}
}
