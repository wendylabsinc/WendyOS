package containerd

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestUDPCheckedRetirementPreservesOwnerAcrossFailureAndRetry(t *testing.T) {
	for _, failureAt := range []string{"interception", "ingress", "both"} {
		t.Run(failureAt, func(t *testing.T) {
			f := newMeshRetirementFixture(t, "api")
			ports := []appconfig.PortMapping{{Host: 29182, Container: 28082, Protocol: "tcp"}, {Host: 29182, Container: 28082, Protocol: "udp"}}
			f.labels = wendyLabels(f.app, "api", "1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16", Ports: ports}}, "isolated", nil)
			r := f.client.meshIngress
			if err := r.ClaimUDPForApp(f.container, f.app, 29182, f.ip, 28082); err != nil {
				t.Fatal(err)
			}
			if err := r.ClaimSource(f.container, f.app, f.ip, "10.99.0.0/16", 7); err != nil {
				t.Fatal(err)
			}
			source, destination := netip.MustParseAddr(f.ip), netip.MustParseAddr("10.99.0.5")
			udpToken, sourceToken := r.UDPToken(29182), r.SourceToken(source, destination, 7)
			if udpToken == 0 || sourceToken == 0 {
				t.Fatal("missing live UDP incarnations")
			}
			// A different service shares the app bridge but retains its own lease,
			// port and source incarnation throughout another service's retirement.
			sibling, siblingIP := ContainerName(f.app, "worker"), "10.41.2.3"
			if ok, err := f.store.Reserve(sibling, "eth0", net.ParseIP(siblingIP), "0"); err != nil || !ok {
				t.Fatal(err)
			}
			if err := r.ClaimUDPForApp(sibling, f.app, 29183, siblingIP, 28083); err != nil {
				t.Fatal(err)
			}
			if err := r.ClaimSource(sibling, f.app, siblingIP, "10.99.0.0/16", 7); err != nil {
				t.Fatal(err)
			}
			siblingUDP, siblingSource := r.UDPToken(29183), r.SourceToken(netip.MustParseAddr(siblingIP), destination, 7)
			f.client.serviceIPs[f.app]["worker"] = siblingIP
			interceptFailure, ingressFailure := errors.New("UDP interception removal busy"), errors.New("UDP port removal busy")
			originalIntercept, originalIngress := removeMeshUDPEgressIntercept, removeMeshUDPIngressPortForward
			t.Cleanup(func() {
				removeMeshUDPEgressIntercept, removeMeshUDPIngressPortForward = originalIntercept, originalIngress
			})
			failing := true
			checkRevoked := func() {
				t.Helper()
				if r.Allowed(29182) || r.UDPToken(29182) != 0 || r.SourceToken(source, destination, 7) != 0 {
					t.Fatal("old admission survived into kernel rule retirement")
				}
				if r.UDPToken(29183) != siblingUDP || r.SourceToken(netip.MustParseAddr(siblingIP), destination, 7) != siblingSource {
					t.Fatal("sibling ownership was revoked")
				}
				if len(f.store.GetByID(f.container, "eth0")) != 1 || len(f.store.GetByID(sibling, "eth0")) != 1 {
					t.Fatal("an allocation was released before security rules")
				}
				if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
					t.Fatal("retry proof removed before security rules")
				}
			}
			removeMeshUDPEgressIntercept = func(ip, cidr, owner string, port int) error {
				checkRevoked()
				if err := originalIntercept(ip, cidr, owner, port); err != nil {
					return err
				}
				if failing && (failureAt == "interception" || failureAt == "both") {
					return interceptFailure
				}
				return nil
			}
			removeMeshUDPIngressPortForward = func(host uint16, ip string, container uint16) error {
				checkRevoked()
				if host != 29182 || container != 28082 || ip != f.ip {
					t.Fatalf("wrong UDP endpoint retired: %d %s %d", host, ip, container)
				}
				f.calls = append(f.calls, "udp-ingress")
				if failing && (failureAt == "ingress" || failureAt == "both") {
					return ingressFailure
				}
				return nil
			}
			// Exercise real deleteOne's refusal: any later Spec/Task/Delete call would
			// panic through this deliberately unused embedded container interface.
			_, err := f.client.deleteOne(context.Background(), retirementContainerForTest{id: f.container, labels: f.labels}, false)
			wantIntercept := failureAt == "interception" || failureAt == "both"
			wantIngress := failureAt == "ingress" || failureAt == "both"
			if err == nil || errors.Is(err, interceptFailure) != wantIntercept || errors.Is(err, ingressFailure) != wantIngress {
				t.Fatalf("aggregate error=%v", err)
			}
			if !slices.Contains(f.calls, "ingress") || !slices.Contains(f.calls, "udp-ingress") || !slices.Contains(f.calls, "udp-intercept") {
				t.Fatalf("not all protocol removals attempted: %v", f.calls)
			}
			if f.client.serviceIPs[f.app]["api"] != f.ip || f.client.serviceIPs[f.app]["worker"] != siblingIP {
				t.Fatal("failed delete discarded retry/sibling cache")
			}
			checkRevoked()
			// A subsequent retry can finish cleanup without reauthorizing old traffic.
			failing = false
			if err := f.client.retireMeshNetwork(f.container, f.labels, f.ip); err != nil {
				t.Fatal(err)
			}
			f.client.mu.Lock()
			f.client.forgetRetiredMeshIP(f.labels)
			f.client.mu.Unlock()
			if _, err := os.Stat(networkSandboxResultPath(f.container)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful retirement kept proof: %v", err)
			}
			if f.client.serviceIPs[f.app]["api"] != "" || f.client.serviceIPs[f.app]["worker"] != siblingIP {
				t.Fatal("successful retirement lost exact cache scope")
			}
			if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
				t.Fatal(err)
			}
			calls := len(f.calls)
			if err := f.client.retireMeshNetwork(f.container, f.labels, ""); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != calls || len(f.store.GetByID(sibling, "eth0")) != 1 {
				t.Fatal("stop then delete touched an already-retired or sibling owner")
			}
			// Reuse the address and published port in a later incarnation. The old
			// receive/send handles must not gain entry to its replacement endpoint.
			if ok, err := f.store.Reserve(f.container, "eth0", net.ParseIP(f.ip), "0"); err != nil || !ok {
				t.Fatal(err)
			}
			if err := r.ClaimUDPForApp(f.container, f.app, 29182, f.ip, 28082); err != nil {
				t.Fatal(err)
			}
			if err := r.ClaimSource(f.container, f.app, f.ip, "10.99.0.0/16", 7); err != nil {
				t.Fatal(err)
			}
			if err := r.WithAuthorizedUDPToken(29182, udpToken, func(netip.AddrPort) error { t.Fatal("old endpoint entered replacement"); return nil }); !errors.Is(err, meshingress.ErrPortDenied) {
				t.Fatal(err)
			}
			if err := r.WithSourceToken(source, destination, 7, sourceToken, func() error { t.Fatal("old source entered replacement"); return nil }); !errors.Is(err, meshingress.ErrPortDenied) {
				t.Fatal(err)
			}
		})
	}
}
