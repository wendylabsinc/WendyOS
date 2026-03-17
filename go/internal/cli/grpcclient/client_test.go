package grpcclient

import (
	"net/url"
	"strings"
	"testing"
)

// ── grpcTarget ──────────────────────────────────────────────────────

func TestGrpcTarget_IPv4(t *testing.T) {
	got := grpcTarget("192.168.1.5:50051")
	if got != "192.168.1.5:50051" {
		t.Fatalf("grpcTarget IPv4 = %q, want %q", got, "192.168.1.5:50051")
	}
}

func TestGrpcTarget_Hostname(t *testing.T) {
	got := grpcTarget("wendyos-otter.local:50051")
	if got != "wendyos-otter.local:50051" {
		t.Fatalf("grpcTarget hostname = %q, want %q", got, "wendyos-otter.local:50051")
	}
}

func TestGrpcTarget_IPv6NoZone(t *testing.T) {
	got := grpcTarget("[2001:db8::1]:50051")
	if got != "[2001:db8::1]:50051" {
		t.Fatalf("grpcTarget IPv6 global = %q, want %q", got, "[2001:db8::1]:50051")
	}
}

func TestGrpcTarget_IPv6Loopback(t *testing.T) {
	got := grpcTarget("[::1]:50051")
	if got != "[::1]:50051" {
		t.Fatalf("grpcTarget IPv6 loopback = %q, want %q", got, "[::1]:50051")
	}
}

func TestGrpcTarget_IPv6LinkLocalWithZone_Bracketed(t *testing.T) {
	got := grpcTarget("[fe80::1%en0]:50051")
	// Brackets are percent-encoded in the URL path (%5B/%5D), zone % becomes %25.
	want := "passthrough:///%5Bfe80::1%25en0%5D:50051"
	if got != want {
		t.Fatalf("grpcTarget bracketed IPv6+zone = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::1%en0]:50051")
}

func TestGrpcTarget_IPv6LinkLocalWithZone_Unbracketed(t *testing.T) {
	got := grpcTarget("fe80::8c13:12bf:4df8:b976%en24:50051")
	want := "passthrough:///%5Bfe80::8c13:12bf:4df8:b976%25en24%5D:50051"
	if got != want {
		t.Fatalf("grpcTarget unbracketed IPv6+zone = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::8c13:12bf:4df8:b976%en24]:50051")
}

func TestGrpcTarget_IPv6LinkLocalWithZone_ShortZone(t *testing.T) {
	got := grpcTarget("[fe80::1%5]:50051")
	want := "passthrough:///%5Bfe80::1%255%5D:50051"
	if got != want {
		t.Fatalf("grpcTarget short zone = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::1%5]:50051")
}

func TestGrpcTarget_IPv6LinkLocalWithZone_LongZone(t *testing.T) {
	// Linux-style zone: eth0, wlan0, etc.
	got := grpcTarget("[fe80::1%eth0]:50051")
	want := "passthrough:///%5Bfe80::1%25eth0%5D:50051"
	if got != want {
		t.Fatalf("grpcTarget linux zone = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::1%eth0]:50051")
}

func TestGrpcTarget_IPv6LinkLocalWithZone_UnbracketedShortAddress(t *testing.T) {
	got := grpcTarget("fe80::1%en0:50051")
	want := "passthrough:///%5Bfe80::1%25en0%5D:50051"
	if got != want {
		t.Fatalf("grpcTarget unbracketed short IPv6+zone = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::1%en0]:50051")
}

func TestGrpcTarget_IPv6LinkLocalWithZone_MTLSPort(t *testing.T) {
	// mTLS port is plaintext + 1.
	got := grpcTarget("[fe80::1%en0]:50052")
	want := "passthrough:///%5Bfe80::1%25en0%5D:50052"
	if got != want {
		t.Fatalf("grpcTarget mTLS port = %q, want %q", got, want)
	}
	assertValidPassthroughURL(t, got, "/[fe80::1%en0]:50052")
}

// ── hostFromAddress ─────────────────────────────────────────────────

func TestHostFromAddress_IPv4(t *testing.T) {
	got := hostFromAddress("192.168.1.5:50051")
	if got != "192.168.1.5" {
		t.Fatalf("hostFromAddress IPv4 = %q, want %q", got, "192.168.1.5")
	}
}

func TestHostFromAddress_Hostname(t *testing.T) {
	got := hostFromAddress("wendyos-otter.local:50051")
	if got != "wendyos-otter.local" {
		t.Fatalf("hostFromAddress hostname = %q, want %q", got, "wendyos-otter.local")
	}
}

func TestHostFromAddress_IPv6WithZone(t *testing.T) {
	got := hostFromAddress("[fe80::1%en0]:50051")
	want := "fe80::1%en0"
	if got != want {
		t.Fatalf("hostFromAddress IPv6+zone = %q, want %q", got, want)
	}
}

func TestHostFromAddress_IPv6Global(t *testing.T) {
	got := hostFromAddress("[2001:db8::1]:50051")
	if got != "2001:db8::1" {
		t.Fatalf("hostFromAddress IPv6 global = %q, want %q", got, "2001:db8::1")
	}
}

func TestHostFromAddress_IPv6Loopback(t *testing.T) {
	got := hostFromAddress("[::1]:50051")
	if got != "::1" {
		t.Fatalf("hostFromAddress IPv6 loopback = %q, want %q", got, "::1")
	}
}

func TestHostFromAddress_NoPort(t *testing.T) {
	// When there's no port, SplitHostPort fails and the address is
	// returned as-is.
	got := hostFromAddress("192.168.1.5")
	if got != "192.168.1.5" {
		t.Fatalf("hostFromAddress no port = %q, want %q", got, "192.168.1.5")
	}
}

// ── helpers ─────────────────────────────────────────────────────────

// assertValidPassthroughURL verifies the target is a valid URL with the
// passthrough scheme and that the path decodes back to wantPath (the
// address prefixed with "/"). gRPC's passthrough resolver reads the
// endpoint from URL.Path, not URL.Host.
func assertValidPassthroughURL(t *testing.T, target, wantPath string) {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("grpcTarget produced invalid URL %q: %v", target, err)
	}
	if parsed.Scheme != "passthrough" {
		t.Fatalf("scheme = %q, want %q", parsed.Scheme, "passthrough")
	}
	if parsed.Host != "" {
		t.Fatalf("parsed Host = %q, want empty (address should be in Path)", parsed.Host)
	}
	if parsed.Path != wantPath {
		t.Fatalf("parsed Path = %q, want %q", parsed.Path, wantPath)
	}
	// Verify gRPC's Endpoint() would return the address (path sans leading /).
	endpoint := strings.TrimPrefix(parsed.Path, "/")
	if endpoint == "" {
		t.Fatal("endpoint is empty — passthrough resolver would reject this target")
	}
}
