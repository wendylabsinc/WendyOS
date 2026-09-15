package ros2camera

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

// Seed participants so reconciliation is exercised without opening DDS sockets
// or requiring Linux network namespaces.
func seedParticipant(m *Manager, iface string, domain int, pid uint32, graphKey, instanceKey string) *participantState {
	p := &participantState{iface: iface, domainID: domain, netnsPID: pid, graphKey: graphKey, cancel: func() {}}
	m.participants[participantKey(iface, domain, pid, instanceKey)] = p
	return p
}

func TestHostNetworkAppReusesRobotCameraAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	graph := Graph{Key: "robot-navigation", InstanceKey: "navigation-old", DomainID: 0, NetworkNamespacePID: 100, HostNetwork: true}
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, path, func(context.Context) ([]Graph, error) {
		return []Graph{graph}, nil
	})
	t.Cleanup(m.Shutdown)
	host := seedParticipant(m, "eth0", 0, 0, "host:eth0", "host")
	loop := seedParticipant(m, "lo", 0, 0, "host:lo", "host")
	oldApp := seedParticipant(m, "", 0, graph.NetworkNamespacePID, graph.Key, graph.InstanceKey)
	endpoint := rtps.Endpoint{Topic: "rt/frontvideostream", Type: TypeGo2FrontVideo, GUID: rtps.GUID{EntityID: 7}}
	m.registerEndpoint(host, endpoint)
	m.registerEndpoint(oldApp, endpoint)
	if got := m.List(); len(got) != 2 {
		t.Fatalf("fixture must reproduce the existing duplicate: %+v", got)
	}

	// Changing the app's container/PID must retain the original host camera
	// and retire discovery created by older versions under the app identity.
	graph.InstanceKey, graph.NetworkNamespacePID = "navigation-new", 200
	m.reconcileInterfaces(context.Background(), []string{"eth0"})
	if len(m.participants) != 2 || host.retired || loop.retired || !oldApp.retired {
		t.Fatalf("host participants were not reused: %+v", m.participants)
	}
	got := m.List()
	if len(got) != 1 || got[0].ID != IDBandStart {
		t.Fatalf("cameras after app restart = %+v; want only the original robot camera", got)
	}
	// A queued announcement cannot revive the app's obsolete duplicate.
	m.registerEndpoint(oldApp, endpoint)
	if got := m.List(); len(got) != 1 {
		t.Fatalf("retired discovery restored the duplicate: %+v", got)
	}

	// An agent update reloads both persisted ID assignments. Only current host
	// discovery is listed, and the original device ID remains unchanged.
	m2 := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, path, nil)
	t.Cleanup(m2.Shutdown)
	m2.registerEndpoint(&participantState{iface: "eth0", graphKey: "host:eth0"}, endpoint)
	if got := m2.List(); len(got) != 1 || got[0].ID != IDBandStart {
		t.Fatalf("cameras after agent restart = %+v", got)
	}
}

func TestHostNetworkAppPreservesLoopbackAndNonzeroDomains(t *testing.T) {
	graphs := []Graph{
		{Key: "first", InstanceKey: "first-container", DomainID: 42, NetworkNamespacePID: 100, HostNetwork: true},
		{Key: "second", InstanceKey: "second-container", DomainID: 42, NetworkNamespacePID: 200, HostNetwork: true},
	}
	m := NewManager(context.Background(), zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), func(context.Context) ([]Graph, error) {
		return graphs, nil
	})
	t.Cleanup(m.Shutdown)
	base := seedParticipant(m, "eth0", 0, 0, "host:eth0", "host")
	wired := seedParticipant(m, "eth0", 42, 0, "host:eth0", "host")
	loop := seedParticipant(m, "lo", 42, 0, "host:lo", "host")
	m.reconcileInterfaces(context.Background(), []string{"eth0"})
	if len(m.participants) != 3 || base.retired || wired.retired || loop.retired {
		t.Fatalf("host domain/loopback discovery was not shared: %+v", m.participants)
	}
	graphs = graphs[1:]
	m.reconcileInterfaces(context.Background(), []string{"eth0"})
	if wired.retired || loop.retired {
		t.Fatal("removing one host app stopped discovery still needed by the other")
	}
	graphs = nil
	m.reconcileInterfaces(context.Background(), []string{"eth0"})
	if len(m.participants) != 1 || base.retired || !wired.retired || !loop.retired {
		t.Fatalf("unused app domain discovery was not retired: %+v", m.participants)
	}
}

func TestIsolatedAppsKeepTheirDiscoveryNamespaces(t *testing.T) {
	graphs := []Graph{
		{Key: "first", InstanceKey: "first-container", DomainID: 0, NetworkNamespacePID: 100},
		{Key: "second", InstanceKey: "second-container", DomainID: 0, NetworkNamespacePID: 200},
	}
	m := NewManager(context.Background(), zap.NewNop(), nil, filepath.Join(t.TempDir(), "registry.json"), func(context.Context) ([]Graph, error) {
		return graphs, nil
	})
	t.Cleanup(m.Shutdown)
	seedParticipant(m, "eth0", 0, 0, "host:eth0", "host")
	for _, graph := range graphs {
		for _, iface := range []string{"lo", ""} {
			seedParticipant(m, iface, graph.DomainID, graph.NetworkNamespacePID, graph.Key, graph.InstanceKey)
		}
	}
	m.reconcileInterfaces(context.Background(), []string{"eth0"})
	if len(m.participants) != 5 {
		t.Fatalf("isolated discovery was merged: %+v", m.participants)
	}
	for _, graph := range graphs {
		p := m.participants[participantKey("", graph.DomainID, graph.NetworkNamespacePID, graph.InstanceKey)]
		if p == nil || p.retired || p.graphKey != graph.Key {
			t.Fatalf("lost isolated graph %+v", graph)
		}
	}
}
