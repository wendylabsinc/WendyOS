package services

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func resolvesTo(addrs ...string) func(context.Context, string) ([]string, error) {
	return func(context.Context, string) ([]string, error) { return addrs, nil }
}

func connects(t *testing.T, dialed *[]string) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, _, address string) (net.Conn, error) {
		*dialed = append(*dialed, address)
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return client, nil
	}
}

// neverDials uses t.Error, not t.Fatal: GetBuildCapabilities runs the check on
// its own goroutine, where FailNow is not allowed.
func neverDials(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	return func(context.Context, string, string) (net.Conn, error) {
		t.Error("must not dial a registry that did not resolve")
		return nil, errors.New("unexpected dial")
	}
}

// dialFailure is the error shape net.Dialer returns, so rootCause is exercised
// on the real wrapping rather than a bare string.
func dialFailure(reason string) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: errors.New(reason)}}
}

func TestCheckRegistryReachable_Reachable(t *testing.T) {
	var dialed []string
	got := checkRegistryReachable(context.Background(), internetProbeHost, internetProbePort,
		resolvesTo("192.0.2.10"), connects(t, &dialed))
	if !got.Reachable || got.Detail != "" {
		t.Fatalf("got %+v, want reachable with no detail", got)
	}
	if len(dialed) != 1 || dialed[0] != "192.0.2.10:443" {
		t.Fatalf("dialed %v, want the resolved address on 443", dialed)
	}
}

// A host with no IPv6 route fails the AAAA address at once; the A record after
// it still proves the registry is reachable.
func TestCheckRegistryReachable_TriesEachResolvedAddress(t *testing.T) {
	var dialed []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "[") {
			dialed = append(dialed, address)
			return nil, dialFailure("network is unreachable")
		}
		return connects(t, &dialed)(ctx, network, address)
	}
	got := checkRegistryReachable(context.Background(), internetProbeHost, internetProbePort,
		resolvesTo("2001:db8::1", "192.0.2.10"), dial)
	if !got.Reachable {
		t.Fatalf("got %+v, want reachable through the second address", got)
	}
	if want := []string{"[2001:db8::1]:443", "192.0.2.10:443"}; strings.Join(dialed, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed %v, want %v", dialed, want)
	}
}

// The case that motivated the check: a Jetson on USB alone, whose only route is
// link-local, cannot resolve the registry at all.
func TestCheckRegistryReachable_Unresolvable(t *testing.T) {
	lookup := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: internetProbeHost, IsNotFound: true}
	}
	got := checkRegistryReachable(context.Background(), internetProbeHost, internetProbePort, lookup, neverDials(t))
	if got.Reachable {
		t.Fatal("an unresolvable registry must not be reported reachable")
	}
	if got.Detail != "cannot resolve registry-1.docker.io" {
		t.Fatalf("detail = %q", got.Detail)
	}
}

func TestCheckRegistryReachable_NoAddresses(t *testing.T) {
	got := checkRegistryReachable(context.Background(), internetProbeHost, internetProbePort, resolvesTo(), neverDials(t))
	if got.Reachable || got.Detail != "cannot resolve registry-1.docker.io" {
		t.Fatalf("got %+v, want an empty answer treated as unresolvable", got)
	}
}

func TestCheckRegistryReachable_ConnectFailureNamesTheReason(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, dialFailure("network is unreachable")
	}
	got := checkRegistryReachable(context.Background(), internetProbeHost, internetProbePort, resolvesTo("192.0.2.10"), dial)
	if got.Reachable {
		t.Fatal("a refused dial must not be reported reachable")
	}
	if want := "cannot connect to registry-1.docker.io:443: network is unreachable"; got.Detail != want {
		t.Fatalf("detail = %q, want %q", got.Detail, want)
	}
}

func TestCheckRegistryReachable_LookupTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lookup := func(ctx context.Context, host string) ([]string, error) {
		<-ctx.Done()
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	got := checkRegistryReachable(ctx, internetProbeHost, internetProbePort, lookup, neverDials(t))
	if got.Reachable || got.Detail != "timed out resolving registry-1.docker.io" {
		t.Fatalf("got %+v", got)
	}
}

// A blackholed route: SYNs go out and nothing answers until the deadline.
func TestCheckRegistryReachable_DialTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	got := checkRegistryReachable(ctx, internetProbeHost, internetProbePort, resolvesTo("192.0.2.10", "192.0.2.11"), dial)
	if got.Reachable || got.Detail != "timed out connecting to registry-1.docker.io:443" {
		t.Fatalf("got %+v", got)
	}
}

// An agent built without the check must look exactly like an older agent, so
// the CLI says nothing rather than reporting the zero value as "offline".
func TestGetBuildCapabilities_NoInternetCheckLooksLikeOlderAgent(t *testing.T) {
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{ResolveTarget: testResolveTarget, ConfigPath: t.TempDir()})
	resp, err := svc.GetBuildCapabilities(context.Background(), &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetBuildCapabilities: %v", err)
	}
	if resp.GetInternetCheckSupported() || resp.GetInternetReachable() || resp.GetInternetDetail() != "" {
		t.Fatalf("got supported=%v reachable=%v detail=%q, want all unset",
			resp.GetInternetCheckSupported(), resp.GetInternetReachable(), resp.GetInternetDetail())
	}
}

func TestGetBuildCapabilities_ReportsInternetReachable(t *testing.T) {
	var dialed []string
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{
		ResolveTarget: testResolveTarget,
		ConfigPath:    enabledConfigDir(t),
		CheckInternet: func(ctx context.Context) InternetStatus {
			return checkRegistryReachable(ctx, internetProbeHost, internetProbePort, resolvesTo("192.0.2.10"), connects(t, &dialed))
		},
	})
	resp, err := svc.GetBuildCapabilities(context.Background(), &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetBuildCapabilities: %v", err)
	}
	if !resp.GetInternetCheckSupported() || !resp.GetInternetReachable() || resp.GetInternetDetail() != "" {
		t.Fatalf("got supported=%v reachable=%v detail=%q", resp.GetInternetCheckSupported(), resp.GetInternetReachable(), resp.GetInternetDetail())
	}
}

// Reported even when BuildKit is absent: a host that cannot reach the internet
// cannot install BuildKit either, which is the reason worth showing.
func TestGetBuildCapabilities_ReportsUnresolvableRegistry(t *testing.T) {
	lookup := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: internetProbeHost, IsNotFound: true}
	}
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{
		ResolveTarget:   testResolveTarget,
		ConfigPath:      enabledConfigDir(t),
		BuildkitAddress: "unix:///nonexistent/buildkitd.sock",
		CheckInternet: func(ctx context.Context) InternetStatus {
			return checkRegistryReachable(ctx, internetProbeHost, internetProbePort, lookup, neverDials(t))
		},
	})
	resp, err := svc.GetBuildCapabilities(context.Background(), &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetBuildCapabilities: %v", err)
	}
	if resp.GetBuildkitAvailable() {
		t.Fatal("test setup: BuildKit should be absent")
	}
	if !resp.GetInternetCheckSupported() || resp.GetInternetReachable() {
		t.Fatalf("got supported=%v reachable=%v, want a supported check reporting unreachable",
			resp.GetInternetCheckSupported(), resp.GetInternetReachable())
	}
	if resp.GetInternetDetail() != "cannot resolve registry-1.docker.io" {
		t.Fatalf("detail = %q", resp.GetInternetDetail())
	}
}

// The RPC must answer within the budget even when a check ignores its context:
// the capability probe runs before every remote build, and a device that
// stalls it would look hung rather than offline.
func TestGetBuildCapabilities_InternetCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	svc := NewBuildService(zap.NewNop(), BuildServiceOptions{
		ResolveTarget: testResolveTarget,
		ConfigPath:    enabledConfigDir(t),
		CheckInternet: func(context.Context) InternetStatus {
			<-release // ignores the deadline on purpose
			return InternetStatus{Reachable: true}
		},
	})
	svc.internetCheckTimeout = 50 * time.Millisecond

	start := time.Now()
	resp, err := svc.GetBuildCapabilities(context.Background(), &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetBuildCapabilities: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GetBuildCapabilities took %v with a 50ms internet budget", elapsed)
	}
	if !resp.GetInternetCheckSupported() || resp.GetInternetReachable() {
		t.Fatalf("got supported=%v reachable=%v, want an unfinished check reported as unreachable",
			resp.GetInternetCheckSupported(), resp.GetInternetReachable())
	}
	if !strings.Contains(resp.GetInternetDetail(), "timed out") {
		t.Fatalf("detail = %q, want it to say the check timed out", resp.GetInternetDetail())
	}
}
