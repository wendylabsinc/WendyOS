package sessionbroker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func TestOperatorLANDiscoveryNormalBoundedCandidatesAndCancellation(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice"
	operator, err := meshoperator.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	previous := browseOperatorLAN
	t.Cleanup(func() { browseOperatorLAN = previous })
	finished := make(chan struct{})
	browseOperatorLAN = func(ctx context.Context, service string) (<-chan discovery.MDNSService, error) {
		if service != operatorLANService {
			t.Fatalf("browsed %s", service)
		}
		ch := make(chan discovery.MDNSService)
		go func() {
			defer close(ch)
			defer close(finished)
			for i := 0; i < 100; i++ {
				svc := discovery.MDNSService{IPAddress: fmt.Sprintf("192.168.1.%d", i+1), Port: 43024, TXTRecords: map[string]string{"platform": "WendyOS", "version": "1", "protocol": "tls-v1", "role": "router", "principal": "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"}}
				select {
				case ch <- svc:
				case <-ctx.Done():
					return
				}
			}
			<-ctx.Done()
		}()
		return ch, nil
	}
	candidates, err := discoverOperatorLAN(context.Background(), operator)
	if err != nil || len(candidates) != operatorLANCandidates {
		t.Fatal("bounded LAN candidate set", len(candidates), err)
	}
	for _, candidate := range candidates {
		if candidate.Principal != principal || candidate.RouterUUID != "22222222-2222-4222-8222-222222222222" {
			t.Fatal("candidate identity", candidate)
		}
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("bounded browse did not stop its driver")
	}
}

func TestOperatorLANDiscoveryNormalIPv6InterfaceScope(t *testing.T) {
	operator, _ := meshoperator.ParsePrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	service := discovery.MDNSService{IPAddress: "fe80::1234", InterfaceName: "en0", Port: 43024, TXTRecords: map[string]string{"platform": "WendyOS", "version": "1", "protocol": "tls-v1", "role": "router", "principal": "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"}}
	candidate, ok := operatorLANCandidate(service, operator)
	if !ok || candidate.RouterAddress != "[fe80::1234%en0]:43024" {
		t.Fatal("scoped IPv6 candidate", candidate, ok)
	}
}
