package containerd

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestPreparedNetworkRollbackPinsRulesAndMetadataUntilRetry(t *testing.T) {
	for _, failureAt := range []string{"tcp", "udp-interception", "udp-ingress"} {
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
			sibling, siblingIP := ContainerName(f.app, "worker"), "10.41.2.3"
			if ok, err := f.store.Reserve(sibling, "eth0", net.ParseIP(siblingIP), "0"); err != nil || !ok {
				t.Fatal(err)
			}
			f.client.serviceIPs[f.app]["worker"] = siblingIP
			if err := r.ClaimUDPForApp(sibling, f.app, 29183, siblingIP, 28083); err != nil {
				t.Fatal(err)
			}
			siblingToken := r.UDPToken(29183)
			oldTCP, oldIntercept, oldUDP := removeMeshIngressPortForward, removeMeshUDPEgressIntercept, removeMeshUDPIngressPortForward
			t.Cleanup(func() {
				removeMeshIngressPortForward, removeMeshUDPEgressIntercept, removeMeshUDPIngressPortForward = oldTCP, oldIntercept, oldUDP
			})
			injected := errors.New("injected rule removal failure")
			failing := true
			removeMeshIngressPortForward = func(host uint16, ip string, port uint16) error {
				if err := oldTCP(host, ip, port); err != nil {
					return err
				}
				if failing && failureAt == "tcp" {
					return injected
				}
				return nil
			}
			removeMeshUDPEgressIntercept = func(ip, cidr, owner string, port int) error {
				if err := oldIntercept(ip, cidr, owner, port); err != nil {
					return err
				}
				if failing && failureAt == "udp-interception" {
					return injected
				}
				return nil
			}
			removeMeshUDPIngressPortForward = func(host uint16, ip string, port uint16) error {
				if host != 29182 || ip != f.ip || port != 28082 {
					t.Fatal("wrong UDP endpoint")
				}
				if len(f.store.GetByID(f.container, "eth0")) != 1 {
					t.Fatal("UDP cleanup ran after address release")
				}
				if failing && failureAt == "udp-ingress" {
					return injected
				}
				return nil
			}
			ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), "rollback-test"))
			cancel()
			releases := 0
			release := func(cleanupCtx context.Context) error {
				releases++
				if cleanupCtx.Err() != nil {
					t.Fatal("cleanup inherited canceled start context")
				}
				if ns, ok := namespaces.Namespace(cleanupCtx); !ok || ns != "rollback-test" {
					t.Fatal("cleanup lost containerd namespace")
				}
				if deadline, ok := cleanupCtx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
					t.Fatal("cleanup lacks independent bounded deadline")
				}
				if f.client.serviceIPs[f.app]["api"] != f.ip {
					t.Fatal("IP cache discarded before CNI release")
				}
				if r.Allowed(29182) || r.AllowedUDP(29182) || r.SourceToken(netip.MustParseAddr(f.ip), netip.MustParseAddr("10.99.0.4"), 7) != 0 {
					t.Fatal("admission survived into address release")
				}
				return f.store.ReleaseByID(f.container, "eth0")
			}
			if err := f.client.rollbackPreparedNetwork(ctx, f.container, f.labels, f.ip, release); !errors.Is(err, injected) {
				t.Fatalf("rollback error=%v", err)
			}
			if releases != 0 || len(f.store.GetByID(f.container, "eth0")) != 1 || f.client.serviceIPs[f.app]["api"] != f.ip {
				t.Fatal("failed rollback released address or metadata")
			}
			if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
				t.Fatal("failed rollback discarded retry proof")
			}
			failing = false
			if err := f.client.rollbackPreparedNetwork(ctx, f.container, f.labels, f.ip, release); err != nil {
				t.Fatal(err)
			}
			if releases != 1 || len(f.store.GetByID(f.container, "eth0")) != 0 || f.client.serviceIPs[f.app]["api"] != "" {
				t.Fatal("successful rollback did not retire exact resources")
			}
			if _, err := os.Stat(networkSandboxResultPath(f.container)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful rollback retained stale proof")
			}
			if len(f.store.GetByID(sibling, "eth0")) != 1 || f.client.serviceIPs[f.app]["worker"] != siblingIP || r.UDPToken(29183) != siblingToken {
				t.Fatal("rollback changed sibling ownership")
			}
		})
	}
}

func TestPreparedNetworkRollbackRejectsUntrustedOwnership(t *testing.T) {
	for _, mode := range []string{"labels", "namespace", "sibling-address"} {
		t.Run(mode, func(t *testing.T) {
			f := newMeshRetirementFixture(t, "api")
			switch mode {
			case "labels":
				f.labels[labelKeyAppID] = "com.example.other"
			case "namespace":
				f.writeProof(t, "/proc/123/ns/net", f.ip)
			case "sibling-address":
				f.writeProof(t, networkSandboxPath(f.container), "10.41.2.3")
			}
			released := false
			if err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error { released = true; return nil }); err == nil {
				t.Fatal("untrusted cleanup succeeded")
			}
			if released || len(f.calls) != 0 || len(f.store.GetByID(f.container, "eth0")) != 1 || f.client.serviceIPs[f.app]["api"] != f.ip {
				t.Fatal("untrusted rollback changed ownership")
			}
			if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
				t.Fatal("untrusted rollback discarded proof")
			}
		})
	}
}

func TestPreparedNetworkRollbackKeepsCacheOnCNIReleaseFailure(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	failure := errors.New("CNI release unavailable")
	err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error { return failure })
	if !errors.Is(err, failure) || len(f.store.GetByID(f.container, "eth0")) != 1 || f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatalf("CNI failure lost retry ownership: %v", err)
	}
	// Kernel retirement completed, so its proof is retired. The exact IPAM
	// owner remains sufficient for a subsequent checked cleanup retry.
	f.requireProof = false
	if err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error { return f.store.ReleaseByID(f.container, "eth0") }); err != nil {
		t.Fatal(err)
	}
	if f.client.serviceIPs[f.app][""] != "" {
		t.Fatal("successful retry retained cache")
	}
}

func TestPreparedNetworkRollbackBeforeProofPersistence(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	// CNI ADD succeeded, but a later route/port preparation step failed before
	// the recovery proof could be persisted. The pinned allocator is authority.
	if err := os.Remove(networkSandboxResultPath(f.container)); err != nil {
		t.Fatal(err)
	}
	f.requireProof = false
	if err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error { return f.store.ReleaseByID(f.container, "eth0") }); err != nil {
		t.Fatal(err)
	}
	if len(f.store.GetByID(f.container, "eth0")) != 0 || f.client.serviceIPs[f.app][""] != "" {
		t.Fatal("partial preparation leaked ownership")
	}
}

func TestPreparedNetworkRollbackReportsActualCanceledDEL(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(cleanupCtx context.Context) error {
		canceled, cancel := context.WithCancel(cleanupCtx)
		cancel()
		return f.client.cniDelChecked(canceled, f.app, f.container, "/proc/123/ns/net")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("actual CNI subprocess failure disappeared: %v", err)
	}
	if len(f.store.GetByID(f.container, "eth0")) != 1 || f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatal("DEL that never launched discarded retry ownership")
	}
	if _, err := os.Stat(networkSandboxResultPath(f.container)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful kernel retirement must not resurrect its old proof")
	}
	f.requireProof = false
	if err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error {
		return f.store.ReleaseByID(f.container, "eth0")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedNetworkPartialDELQuarantinesStaleCacheWithoutStealingReusedIP(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	partialFailure := errors.New("CNI DEL failed after releasing IPAM")
	err := f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error {
		if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
			t.Fatal(err)
		}
		return partialFailure
	})
	if !errors.Is(err, partialFailure) || f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatalf("partial DEL lost its failure or retry cache: %v", err)
	}
	if _, err := os.Stat(networkSandboxResultPath(f.container)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial DEL reintroduced retired proof")
	}
	sibling := ContainerName(f.app, "replacement-owner")
	if ok, err := f.store.Reserve(sibling, "eth0", net.ParseIP(f.ip), "0"); err != nil || !ok {
		t.Fatal(err)
	}
	if err := f.client.meshIngress.ClaimUDPForApp(sibling, f.app, 29183, f.ip, 28083); err != nil {
		t.Fatal(err)
	}
	siblingToken := f.client.meshIngress.UDPToken(29183)
	previousCalls := len(f.calls)
	released := false
	err = f.client.rollbackPreparedNetwork(context.Background(), f.container, f.labels, f.ip, func(context.Context) error {
		released = true
		return nil
	})
	if err == nil || released || len(f.calls) != previousCalls {
		t.Fatal("stale cached address authorized further kernel/CNI teardown")
	}
	if len(f.store.GetByID(sibling, "eth0")) != 1 || f.client.meshIngress.UDPToken(29183) != siblingToken {
		t.Fatal("retry touched the new address owner")
	}
	if f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatal("ambiguous partial cleanup silently discarded quarantine evidence")
	}
}
