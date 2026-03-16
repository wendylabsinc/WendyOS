package grpcclient

import (
	"net/url"
	"testing"
)

func TestGrpcTarget_IPv4(t *testing.T) {
	got := grpcTarget("192.168.1.5:50051")
	if got != "192.168.1.5:50051" {
		t.Fatalf("grpcTarget IPv4 = %q, want %q", got, "192.168.1.5:50051")
	}
}

func TestGrpcTarget_IPv6LinkLocalWithZone(t *testing.T) {
	got := grpcTarget("[fe80::1%en0]:50051")
	want := "passthrough:///[fe80::1%25en0]:50051"
	if got != want {
		t.Fatalf("grpcTarget IPv6+zone = %q, want %q", got, want)
	}
	// The target must be parseable as a URL.
	if _, err := url.Parse(got); err != nil {
		t.Fatalf("grpcTarget produced invalid URL: %v", err)
	}
}

func TestGrpcTarget_IPv6NoZone(t *testing.T) {
	got := grpcTarget("[2001:db8::1]:50051")
	if got != "[2001:db8::1]:50051" {
		t.Fatalf("grpcTarget IPv6 global = %q, want %q", got, "[2001:db8::1]:50051")
	}
}

func TestHostFromAddress_IPv6WithZone(t *testing.T) {
	got := hostFromAddress("[fe80::1%en0]:50051")
	want := "fe80::1%en0"
	if got != want {
		t.Fatalf("hostFromAddress IPv6+zone = %q, want %q", got, want)
	}
}
