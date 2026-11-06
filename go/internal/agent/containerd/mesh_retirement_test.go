package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/disk"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
)

type meshRetirementFixture struct {
	app, container, ip string
	labels             map[string]string
	store              *disk.Store
	client             *Client
	calls              []string
	requireProof       bool
}

func newMeshRetirementFixture(t *testing.T, service string) *meshRetirementFixture {
	t.Helper()
	withTempSubnetRegistry(t)
	oldDir := cniNetnsBindDir
	cniNetnsBindDir = t.TempDir()
	t.Cleanup(func() { cniNetnsBindDir = oldDir })
	f := &meshRetirementFixture{app: "com.example.retirement", ip: "10.41.2.2", requireProof: true}
	f.container = ContainerName(f.app, service)
	data, _ := json.Marshal(map[string]string{f.app: "10.41.2.0/28"})
	if err := os.WriteFile(cniSubnetRegistryPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = disk.New("wendy-"+f.app, filepath.Join(filepath.Dir(cniSubnetRegistryPath), f.app))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	if ok, err := f.store.Reserve(f.container, "eth0", net.ParseIP(f.ip), "0"); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	f.writeProof(t, networkSandboxPath(f.container), f.ip)
	f.labels = wendyLabels(f.app, service, "1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16", Ports: []appconfig.PortMapping{{Host: 29182, Container: 28082}}}}, "isolated", nil)
	f.client = &Client{logger: zap.NewNop(), meshIngress: meshingress.NewRegistry(), serviceIPs: map[string]map[string]string{f.app: {service: f.ip}}}
	if err := f.client.meshIngress.Claim(f.container, 29182, f.ip, 28082); err != nil {
		t.Fatal(err)
	}
	oldRule, oldRedirect, oldPort := removeMeshEgressRule, removeMeshEgressRedirect, removeMeshIngressPortForward
	oldUDPIntercept := removeMeshUDPEgressIntercept
	t.Cleanup(func() {
		removeMeshEgressRule, removeMeshEgressRedirect, removeMeshIngressPortForward = oldRule, oldRedirect, oldPort
		removeMeshUDPEgressIntercept = oldUDPIntercept
	})
	check := func(label, ip string) error {
		if ip != f.ip {
			t.Fatalf("removed another owner's address: %q", ip)
		}
		if len(f.store.GetByID(f.container, "eth0")) != 1 {
			t.Fatal("allocation released before security rules")
		}
		if f.requireProof {
			if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
				t.Fatal("CNI proof removed before security rules")
			}
		}
		if f.client.meshIngress.Allowed(29182) {
			t.Fatal("admission survived rule retirement")
		}
		f.calls = append(f.calls, label)
		return nil
	}
	removeMeshEgressRule = func(ip, cidr string) error { return check("accept", ip) }
	removeMeshEgressRedirect = func(ip, cidr string, port int) error { return check("redirect", ip) }
	removeMeshIngressPortForward = func(host uint16, ip string, port uint16) error { return check("ingress", ip) }
	removeMeshUDPEgressIntercept = func(ip, cidr, owner string, port int) error {
		if owner != f.container {
			t.Fatalf("UDP interception used shared bridge rather than exact container: %q", owner)
		}
		return check("udp-intercept", ip)
	}
	return f
}
func (f *meshRetirementFixture) writeProof(t *testing.T, path, ip string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"interfaces": []map[string]any{{"name": "eth0", "sandbox": path}}, "ips": []map[string]any{{"address": ip + "/28"}}})
	if err := os.WriteFile(networkSandboxResultPath(f.container), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestMeshProofRetirementUsesPinnedExactOwnerBeforeDEL(t *testing.T) {
	for _, service := range []string{"", "api"} {
		t.Run("service="+service, func(t *testing.T) {
			f := newMeshRetirementFixture(t, service)
			// A sibling's address in the same subnet must never be selected by a
			// guessed prefix or by an absent post-restart client cache.
			sibling := ContainerName(f.app, "other")
			if ok, err := f.store.Reserve(sibling, "eth0", net.ParseIP("10.41.2.3"), "0"); err != nil || !ok {
				t.Fatal(err)
			}
			if err := f.client.retireMeshNetwork(f.container, f.labels, ""); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 4 {
				t.Fatalf("retirement calls=%v", f.calls)
			}
			if _, err := os.Stat(networkSandboxResultPath(f.container)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("proof not retired: %v", err)
			}
			// Only the caller's subsequent CNI DEL may release the allocation. A
			// stop followed by delete is safe once its retired cache is cleared.
			if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
				t.Fatal(err)
			}
			f.client.forgetRetiredMeshIP(f.labels)
			if cached := f.client.serviceIPs[f.app][service]; cached != "" {
				t.Fatalf("retired IP remains cached: %q", cached)
			}
			if err := f.client.retireMeshNetwork(f.container, f.labels, f.client.serviceIPs[f.app][service]); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 4 || len(f.store.GetByID(sibling, "eth0")) != 1 {
				t.Fatal("repeat cleanup touched a sibling")
			}
		})
	}
}
func TestMeshRetirementFailureKeepsProofAndAllocationForRetry(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	failure := errors.New("iptables unavailable")
	removeMeshEgressRedirect = func(string, string, int) error { return failure }
	if err := f.client.retireMeshNetwork(f.container, f.labels, f.ip); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if f.client.meshIngress.Allowed(29182) {
		t.Fatal("failed retirement retained admission")
	}
	if len(f.store.GetByID(f.container, "eth0")) != 1 {
		t.Fatal("failed retirement released IPAM")
	}
	if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
		t.Fatal("failed retirement lost retry proof")
	}
	removeMeshEgressRedirect = func(string, string, int) error { return nil }
	if err := f.client.retireMeshNetwork(f.container, f.labels, ""); err != nil {
		t.Fatal(err)
	}
}
func TestMeshRetirementRejectsUntrustedProofAndOwnership(t *testing.T) {
	for _, mode := range []string{"namespace", "sibling-IP", "cached-IP", "labels", "unowned-proof", "ambiguous-lease", "lease-symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newMeshRetirementFixture(t, "")
			cached := f.ip
			switch mode {
			case "namespace":
				f.writeProof(t, "/proc/123/ns/net", f.ip)
			case "sibling-IP":
				f.writeProof(t, networkSandboxPath(f.container), "10.41.2.3")
			case "cached-IP":
				cached = "10.41.2.3"
			case "labels":
				f.labels[labelKeyAppID] = "com.example.other"
			case "unowned-proof":
				if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
					t.Fatal(err)
				}
			case "ambiguous-lease":
				if ok, err := f.store.Reserve(f.container, "eth0", net.ParseIP("10.41.2.4"), "0"); err != nil || !ok {
					t.Fatal(err)
				}
			case "lease-symlink":
				path := filepath.Join(filepath.Dir(cniSubnetRegistryPath), f.app, "wendy-"+f.app, f.ip)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.retireMeshNetwork(f.container, f.labels, cached); err == nil {
				t.Fatal("untrusted retirement succeeded")
			}
			if len(f.calls) != 0 {
				t.Fatalf("untrusted retirement changed rules: %v", f.calls)
			}
			if _, err := os.Stat(networkSandboxResultPath(f.container)); err != nil {
				t.Fatal("untrusted proof was discarded")
			}
		})
	}
}
func TestMeshRetirementAllowsFreshAppBeforeSubnetAllocation(t *testing.T) {
	withTempSubnetRegistry(t)
	data, _ := json.Marshal(map[string]string{"other": "10.1.0.0/28"})
	if err := os.WriteFile(cniSubnetRegistryPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{meshIngress: meshingress.NewRegistry()}
	labels := wendyLabels("newapp", "", "1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "mesh"}}, "isolated", nil)
	if err := c.retireMeshNetwork("newapp", labels, ""); err != nil {
		t.Fatal(err)
	}
}

type retirementContainerForTest struct {
	containerd.Container
	id     string
	labels map[string]string
}

func (f retirementContainerForTest) ID() string { return f.id }
func (f retirementContainerForTest) Labels(context.Context) (map[string]string, error) {
	return f.labels, nil
}
func TestDeleteRefusesToDiscardContainerWhenRuleRetirementFails(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	failure := errors.New("firewall busy")
	removeMeshEgressRule = func(string, string) error { return failure }
	// Any later Spec, Task, or Delete call would panic through the embedded
	// unused interface. The real deleteOne must return before those effects.
	_, err := f.client.deleteOne(context.Background(), retirementContainerForTest{id: f.container, labels: f.labels}, false)
	if !errors.Is(err, failure) {
		t.Fatalf("delete error=%v", err)
	}
	if len(f.store.GetByID(f.container, "eth0")) != 1 {
		t.Fatal("failed delete freed address")
	}
	if f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatal("failed delete discarded cached retry ownership")
	}
}

type retirementUnknownTaskForTest struct{ retirementContainerForTest }

func (f retirementUnknownTaskForTest) Spec(context.Context) (*specs.Spec, error) {
	return &specs.Spec{}, nil
}
func (f retirementUnknownTaskForTest) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return nil, context.Canceled
}
func TestDeleteUnknownTaskKeepsAllocationPinned(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	_, err := f.client.deleteOne(context.Background(), retirementUnknownTaskForTest{retirementContainerForTest{id: f.container, labels: f.labels}}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(f.store.GetByID(f.container, "eth0")) != 1 {
		t.Fatal("unknown task released an address")
	}
	if len(f.calls) != 4 {
		t.Fatalf("exact rules were not retired before checking task: %v", f.calls)
	}
	if f.client.serviceIPs[f.app][""] != "" {
		t.Fatal("successful rule retirement retained a stale cached address")
	}
}

func TestMeshRetirementRejectsCachedAddressWithMissingOwnership(t *testing.T) {
	f := newMeshRetirementFixture(t, "")
	if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(networkSandboxResultPath(f.container)); err != nil {
		t.Fatal(err)
	}
	if err := f.client.retireMeshNetwork(f.container, f.labels, f.ip); err == nil {
		t.Fatal("missing ownership was treated as an already-retired live address")
	}
	if len(f.calls) != 0 || f.client.serviceIPs[f.app][""] != f.ip {
		t.Fatal("unproven retirement changed rules or discarded retry state")
	}
	if f.client.meshIngress.Allowed(29182) {
		t.Fatal("unproven retirement retained ingress admission")
	}
}

// A real containerd client over its supported store seam exercises the entire
// StopContainer group epilogue without a daemon or any host network changes.
type retirementContainerStore struct {
	containers.Store
	container containers.Container
}

func (s retirementContainerStore) Get(context.Context, string) (containers.Container, error) {
	return s.container, nil
}
func (s retirementContainerStore) List(context.Context, ...string) ([]containers.Container, error) {
	return []containers.Container{s.container}, nil
}

func TestWholeAppStopRetainsOwnershipAfterRetirementRefusal(t *testing.T) {
	for _, mode := range []string{"missing-ownership", "firewall-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newMeshRetirementFixture(t, "")
			backend, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(retirementContainerStore{
				container: containers.Container{ID: f.container, Labels: f.labels},
			})))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { backend.Close() })
			f.client.client = backend
			f.client.namespace = "default"
			f.client.appIsolation = map[string]string{f.app: "isolated"}
			f.client.primaryPIDs = map[string]uint32{f.app: 123}
			f.client.appServices = map[string]map[string]*appconfig.ServiceConfig{f.app: {"api": {}}}
			if mode == "missing-ownership" {
				// Both pieces of persisted proof vanished while the cache still
				// records this live address. Every retry must continue to refuse.
				if err := f.store.ReleaseByID(f.container, "eth0"); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(networkSandboxResultPath(f.container)); err != nil {
					t.Fatal(err)
				}
			} else {
				removeMeshEgressRule = func(string, string) error { return errors.New("firewall busy") }
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := f.client.StopContainer(context.Background(), f.app); err == nil {
					t.Fatal("whole-app stop accepted missing live ownership")
				}
				if f.client.serviceIPs[f.app][""] != f.ip || f.client.appIsolation[f.app] != "isolated" || f.client.primaryPIDs[f.app] != 123 || len(f.client.appServices[f.app]) != 1 {
					t.Fatal("failed group stop discarded retry or live sibling state")
				}
				if f.client.appStopping[f.app] {
					t.Fatal("failed group stop blocked a subsequent retry")
				}
			}
			if mode == "missing-ownership" && len(f.calls) != 0 || f.client.meshIngress.Allowed(29182) {
				t.Fatal("unproven whole-app stop changed rules or retained admission")
			}
		})
	}
}
