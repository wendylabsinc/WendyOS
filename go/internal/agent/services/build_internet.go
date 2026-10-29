package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// internetProbeHost and internetProbePort name the registry a build host must
// reach. Docker Hub is the default base-image source: an unqualified FROM line
// resolves there, so it is the one host nearly every build needs.
const (
	internetProbeHost = "registry-1.docker.io"
	internetProbePort = "443"
)

// defaultInternetCheckTimeout bounds the whole check, lookup and dial together.
// GetBuildCapabilities is called on every `wendy run --build-host` preflight, so
// an offline device has to answer in about this long rather than in the
// resolver's own retry budget, which is several times longer.
const defaultInternetCheckTimeout = 2 * time.Second

// InternetStatus is the outcome of an internet reachability check.
type InternetStatus struct {
	Reachable bool
	// Detail is empty when Reachable. Otherwise it is a short reason phrased for
	// a developer, e.g. "cannot resolve registry-1.docker.io".
	Detail string
}

// InternetCheck reports whether this host can reach the internet a build
// needs. It is handed a context with a deadline and should honour it; the
// caller abandons a check that does not.
type InternetCheck func(ctx context.Context) InternetStatus

// CheckRegistryReachable resolves and TCP-dials Docker Hub's registry. It is
// the production InternetCheck.
//
// A TCP connect is the whole test: no TLS handshake and no HTTP request, so the
// check sends nothing and needs no credentials. What it answers is the question
// that actually broke: a device connected over USB alone has a link-local route
// and nothing else, so it cannot resolve the registry, let alone reach it.
//
// It does not consult a proxy or a registry mirror configured for buildkitd, so
// a host that reaches the internet only through one reads as not reachable.
// That is why the result is advisory.
func CheckRegistryReachable(ctx context.Context) InternetStatus {
	dialer := &net.Dialer{}
	return checkRegistryReachable(ctx, internetProbeHost, internetProbePort,
		net.DefaultResolver.LookupHost, dialer.DialContext)
}

// checkRegistryReachable is CheckRegistryReachable with its resolver and dialer
// injected, so each failure can be produced in a test without a network.
func checkRegistryReachable(
	ctx context.Context,
	host, port string,
	lookup func(ctx context.Context, host string) ([]string, error),
	dial func(ctx context.Context, network, address string) (net.Conn, error),
) InternetStatus {
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		if probeTimedOut(ctx, err) {
			return InternetStatus{Detail: "timed out resolving " + host}
		}
		return InternetStatus{Detail: "cannot resolve " + host}
	}

	// Each resolved address in turn: a host without an IPv6 route fails an AAAA
	// address at once with "network is unreachable" and should still pass on
	// the A record that follows it.
	var lastErr error
	for _, addr := range addrs {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(addr, port))
		if err == nil {
			_ = conn.Close()
			return InternetStatus{Reachable: true}
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	target := net.JoinHostPort(host, port)
	if probeTimedOut(ctx, lastErr) {
		return InternetStatus{Detail: "timed out connecting to " + target}
	}
	return InternetStatus{Detail: fmt.Sprintf("cannot connect to %s: %v", target, rootCause(lastErr))}
}

// probeTimedOut reports whether a probe step failed because it ran out of time
// rather than because it was refused.
func probeTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// rootCause strips the "dial tcp 1.2.3.4:443: connect:" wrapping a dial error
// carries, leaving the part a developer acts on ("network is unreachable",
// "connection refused"). The detail already names the registry.
func rootCause(err error) error {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		err = inner
	}
}

// internetStatus runs the configured check under its time budget.
//
// The check gets a deadline and is expected to honour it, but the RPC does not
// rely on that: a check that ignores its context is abandoned at the deadline,
// so an offline device can slow GetBuildCapabilities by the budget and no more.
func (s *BuildService) internetStatus(ctx context.Context) InternetStatus {
	ctx, cancel := context.WithTimeout(ctx, s.internetCheckTimeout)
	defer cancel()
	done := make(chan InternetStatus, 1)
	go func() { done <- s.checkInternet(ctx) }()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		return InternetStatus{Detail: "timed out checking internet reachability"}
	}
}
