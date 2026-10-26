package localmesh

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestKernelOperationError(t *testing.T) {
	for _, test := range []struct {
		name        string
		operation   string
		requirement string
		err         error
		want        string
		absent      string
	}{
		{"ipv6 policy unavailable", "inspect IPv6 policy routing", kernelPolicyRequirement, syscall.EAFNOSUPPORT, "CONFIG_IPV6_MULTIPLE_TABLES", "CAP_NET_ADMIN"},
		{"dummy unavailable", "create dummy interface wlmesh0", kernelDummyRequirement, syscall.EOPNOTSUPP, "CONFIG_DUMMY", "CAP_NET_ADMIN"},
		{"tun absent", "inspect TUN device capabilities", kernelTUNRequirement, syscall.ENOENT, "CONFIG_TUN", "CAP_NET_ADMIN"},
		{"wrapped unsupported", "create peer TUN", kernelTUNRequirement, fmt.Errorf("ioctl: %w", syscall.ENODEV), "CONFIG_TUN", "CAP_NET_ADMIN"},
		{"permission denied", "create dummy interface wlmesh0", kernelDummyRequirement, syscall.EPERM, "CAP_NET_ADMIN", "CONFIG_DUMMY"},
		{"device permission denied", "inspect TUN device capabilities", kernelTUNRequirement, syscall.EACCES, "permissions", "CONFIG_TUN"},
		{"foreign link", "create dummy interface wlmesh0", kernelDummyRequirement, syscall.EEXIST, "file exists", "CONFIG_DUMMY"},
		{"invalid operation", "install IPv6 policy rule", kernelPolicyRequirement, syscall.EINVAL, "invalid argument", "CONFIG_IPV6_MULTIPLE_TABLES"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := kernelOperationError(test.operation, test.requirement, test.err)
			if !errors.Is(err, test.err) {
				t.Fatalf("lost original error: %v", err)
			}
			if !strings.Contains(err.Error(), test.operation) || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), test.absent) {
				t.Fatalf("unexpected diagnostic: %v", err)
			}
		})
	}
	if err := kernelOperationError("success", kernelTUNRequirement, nil); err != nil {
		t.Fatal(err)
	}
}

func TestKernelPreflightStopsAtUnavailableCapability(t *testing.T) {
	var calls []string
	err := runKernelPreflight([]kernelCapabilityCheck{
		{"IPv6", kernelIPv6Requirement, func() error { calls = append(calls, "IPv6"); return nil }},
		{"policy routing", kernelPolicyRequirement, func() error { calls = append(calls, "policy"); return syscall.EAFNOSUPPORT }},
		{"TUN", kernelTUNRequirement, func() error { t.Fatal("continued after unsupported policy routing"); return nil }},
	})
	if !errors.Is(err, syscall.EAFNOSUPPORT) || !strings.Contains(err.Error(), kernelPolicyRequirement) {
		t.Fatalf("missing policy diagnostic: %v", err)
	}
	if strings.Join(calls, ",") != "IPv6,policy" {
		t.Fatalf("unexpected check order: %v", calls)
	}
}

func TestKernelPreflightAvailableCapabilities(t *testing.T) {
	checks := 0
	if err := runKernelPreflight([]kernelCapabilityCheck{
		{"IPv6", kernelIPv6Requirement, func() error { checks++; return nil }},
		{"policy", kernelPolicyRequirement, func() error { checks++; return nil }},
		{"TUN", kernelTUNRequirement, func() error { checks++; return nil }},
	}); err != nil || checks != 3 {
		t.Fatalf("checks=%d, error=%v", checks, err)
	}
}
