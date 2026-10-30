package vpn

import (
	"context"
	"crypto/tls"
	"time"
)

// InternetReachable runs inside the packet tunnel provider. Provider-originated
// sockets use the underlying network, including while a default tunnel route is
// installed. Literal addresses avoid measuring the tunnel's own DNS resolver.
// Two independent TLS endpoints avoid treating one service failure or a captive
// portal response as evidence that the normal connection is usable.
func InternetReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result := make(chan bool, 4)
	for _, target := range []struct{ address, name string }{{"1.1.1.1:443", "cloudflare-dns.com"}, {"8.8.8.8:443", "dns.google"}, {"[2606:4700:4700::1111]:443", "cloudflare-dns.com"}, {"[2001:4860:4860::8888]:443", "dns.google"}} {
		go func() {
			dialer := tls.Dialer{Config: &tls.Config{ServerName: target.name, MinVersion: tls.VersionTLS12}}
			conn, err := dialer.DialContext(ctx, "tcp", target.address)
			if err == nil {
				conn.Close()
			}
			result <- err == nil
		}()
	}
	for range 4 {
		select {
		case ok := <-result:
			if ok {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// ReachabilityState requires two consecutive results to avoid route churn on
// one dropped probe. Start on the normal connection; failover is opt-out only
// by disabling the VPN, never by selecting individual devices or gateways.
type ReachabilityState struct {
	Offline             bool
	successes, failures int
}

func (s *ReachabilityState) Observe(online bool) bool {
	if online {
		s.failures = 0
		s.successes++
		if s.successes >= 2 {
			s.Offline = false
			s.successes = 2
		}
	} else {
		s.successes = 0
		s.failures++
		if s.failures >= 2 {
			s.Offline = true
			s.failures = 2
		}
	}
	return !s.Offline
}
