package meshingress

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestUDPClaimsAreProtocolAndIncarnationScoped(t *testing.T) {
	r := NewRegistry()
	if err := r.ClaimForApp("tcp", "com.wendy.tcp", 7777, "10.3.0.2", 7777); err != nil {
		t.Fatal(err)
	}
	if r.AllowedUDP(7777) {
		t.Fatal("TCP claim authorized UDP")
	}
	if err := r.ClaimUDPForApp("app-a", "com.wendy.a", 7777, "10.3.0.2", 7777); err != nil {
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
	if err := r.ClaimUDPForApp("app-b", "com.wendy.b", 7777, "10.3.0.2", 7777); err != nil {
		t.Fatal(err)
	}
	if err := r.ClaimSource("app-b", "com.wendy.b", ip.String(), "10.99.0.0/16", 7); err != nil {
		t.Fatal(err)
	}
	if r.UDPToken(7777) == a || r.SourceToken(ip, dst, 7) == sourceA {
		t.Fatal("replacement app inherited old flow token")
	}
	called := false
	if err := r.WithAuthorizedUDPToken(7777, a, func(address netip.AddrPort) error { called = true; return nil }); !errors.Is(err, ErrPortDenied) || called {
		t.Fatal("old app UDP flow entered replacement app")
	}
	if err := r.WithAuthorizedUDPToken(7777, r.UDPToken(7777), func(address netip.AddrPort) error { called = true; return nil }); err != nil || !called {
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

func TestUDPClaimPinsContainerEndpoint(t *testing.T) {
	r := NewRegistry()
	if err := r.ClaimUDPForApp("app", "app", 7777, "127.0.0.1", 80); err == nil {
		t.Fatal("UDP authorized host loopback")
	}
	if err := r.ClaimUDPForApp("app", "app", 7777, "10.3.0.2", 80); err != nil {
		t.Fatal(err)
	}
	token := r.UDPToken(7777)
	if err := r.WithAuthorizedUDPToken(7777, token, func(address netip.AddrPort) error {
		if address.String() != "10.3.0.2:80" {
			t.Fatalf("wrong UDP endpoint %v", address)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ClaimUDPForApp("app", "app", 7777, "10.3.0.3", 81); err == nil {
		t.Fatal("live UDP grant retargeted")
	}
	r.Release("app")
	if err := r.WithAuthorizedUDPToken(7777, token, func(netip.AddrPort) error { t.Fatal("revoked UDP endpoint used"); return nil }); err == nil {
		t.Fatal("UDP grant survived revocation")
	}
}

func TestUDPLeaseNotificationsAndCombinedLocalAdmission(t *testing.T) {
	r := NewRegistry()
	ip, dst := netip.MustParseAddr("10.79.42.2"), netip.MustParseAddr("10.99.0.1")
	claim := func() {
		t.Helper()
		if err := r.ClaimSource("source", "app", ip.String(), "10.99.0.0/16", 7); err != nil {
			t.Fatal(err)
		}
		if err := r.ClaimUDPForApp("destination", "app", 7777, "10.3.0.2", 8888); err != nil {
			t.Fatal(err)
		}
	}
	claim()
	source, udp := r.SourceToken(ip, dst, 7), r.UDPToken(7777)
	sd, ud := r.SourceDone(ip, dst, 7, source), r.UDPDone(7777, udp)
	select {
	case <-sd:
		t.Fatal("new source already revoked")
	default:
	}
	select {
	case <-ud:
		t.Fatal("new destination already revoked")
	default:
	}
	entered, finish, sendDone, releaseDone := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		sendDone <- r.WithAuthorizedLocalUDP(ip, dst, 7, source, 7777, udp, func(target netip.AddrPort) error {
			if target.String() != "10.3.0.2:8888" {
				return errors.New("wrong target")
			}
			close(entered)
			<-finish
			return nil
		})
	}()
	<-entered
	go func() { r.Release("source"); close(releaseDone) }()
	select {
	case <-releaseDone:
		t.Fatal("release crossed combined admission")
	case <-time.After(20 * time.Millisecond):
	}
	// A queued writer cannot deadlock another combined admission: there is no
	// outer registry read lock for it to retain while waiting for this writer.
	later := make(chan error, 1)
	go func() {
		later <- r.WithAuthorizedLocalUDP(ip, dst, 7, source, 7777, udp, func(netip.AddrPort) error { return nil })
	}()
	close(finish)
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-releaseDone:
	case <-time.After(time.Second):
		t.Fatal("release deadlocked")
	}
	select {
	case err := <-later:
		if !errors.Is(err, ErrPortDenied) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("combined admission nested read locks")
	}
	select {
	case <-sd:
	default:
		t.Fatal("source lease not notified")
	}
	select {
	case <-ud:
		t.Fatal("independent destination lease revoked")
	default:
	}
	r.Release("destination")
	select {
	case <-ud:
	default:
		t.Fatal("destination lease not notified")
	}
	claim()
	if r.SourceToken(ip, dst, 7) == source || r.UDPToken(7777) == udp {
		t.Fatal("replacement reused incarnation")
	}
	called := false
	if err := r.WithAuthorizedLocalUDP(ip, dst, 7, source, 7777, udp, func(netip.AddrPort) error { called = true; return nil }); !errors.Is(err, ErrPortDenied) || called {
		t.Fatal("old combined tokens accepted")
	}
}
