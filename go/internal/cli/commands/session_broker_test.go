package commands

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestConnectPinnedSessionKeysBrokerByRequestedEndpoint(t *testing.T) {
	oldLoad, oldConnect := loadConfigForPinFn, connectSessionBrokerFn
	t.Cleanup(func() {
		loadConfigForPinFn = oldLoad
		connectSessionBrokerFn = oldConnect
	})
	loadConfigForPinFn = func() (*config.Config, error) {
		return &config.Config{DevicePins: map[string]config.DevicePin{
			"orin": {OrgID: 7, AssetID: "42"},
		}}, nil
	}
	want := &grpcclient.AgentConnection{}
	var gotKeys []string
	connectSessionBrokerFn = func(_ context.Context, key string, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		gotKeys = append(gotKeys, key)
		if expected != (certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}) {
			t.Fatalf("expected identity = %+v", expected)
		}
		return want, nil
	}

	got, ok := connectPinnedSession(context.Background(), "orin.local:50051")
	if !ok || got != want {
		t.Fatalf("connectPinnedSession = (%p, %v), want (%p, true)", got, ok, want)
	}
	// A dev agent on :51000 beside the production agent on the default port
	// presents the same asset certificate, so the identity check cannot tell
	// them apart — only the endpoint in the key can. Host-only keys let an
	// explicit-port request hit (and mutate) the wrong agent.
	if _, ok := connectPinnedSession(context.Background(), "orin.local:51000"); !ok {
		t.Fatal("explicit-port consult failed")
	}
	if len(gotKeys) != 2 || gotKeys[0] != "orin.local:50051" || gotKeys[1] != "orin.local:51000" {
		t.Fatalf("broker keys = %q, want the full requested endpoints", gotKeys)
	}
}

// A broker hit is a liveness-proven connect like any other, and every
// proof-of-life exit must refresh the discovery/LKG cache entry (see the
// comment above cacheConnectSuccess): without it a developer broker-hitting
// every ~90s for two hours watches LastSeen age past the freshness horizon
// despite continuous successful connects, pickers go stale, and the
// post-broker cold start loses the fast-connect quality the cache exists for.
func TestResolveTargetBrokerHitRefreshesDeviceCache(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"orin": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	path := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, path, discoverycache.Entry{ID: "dev-1", Hostname: "orin.local", IP: "10.0.0.5", Port: 50052, MTLS: true})
	origCache, origConnect, origUpdate := deviceCacheLoadFn, connectSessionBrokerFn, checkAndOfferUpdateFn
	t.Cleanup(func() {
		deviceCacheLoadFn, connectSessionBrokerFn, checkAndOfferUpdateFn = origCache, origConnect, origUpdate
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }
	identity := certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}
	connectSessionBrokerFn = func(ctx context.Context, _ string, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		if expected != identity {
			t.Fatalf("expected identity = %+v", expected)
		}
		// The broker's device moved to a new DHCP lease since the cache entry.
		return grpcclient.ConnectSessionProxy(ctx, filepath.Join(t.TempDir(), "unused.sock"), "10.0.0.9", "10.0.0.9:50052", &config.CertificateInfo{OrganizationID: 7}, identity)
	}
	checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		return conn, nil
	}

	sel, err := resolveTargetInner(context.Background(), SelectDevice("orin.local"))
	if err != nil {
		t.Fatalf("resolveTargetInner: %v", err)
	}
	defer sel.Agent.Close()
	if !sel.Agent.IsSessionProxy {
		t.Fatal("expected the broker connection to be used")
	}

	after, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cachedDeviceEntry(after, "orin.local")
	if !ok {
		t.Fatal("cache entry missing after broker hit")
	}
	if e.IP != "10.0.0.9" || e.Port != 50052 || !e.MTLS {
		t.Fatalf("cache entry = {IP:%s Port:%d MTLS:%v}, want the broker session's endpoint {IP:10.0.0.9 Port:50052 MTLS:true} refreshed in", e.IP, e.Port, e.MTLS)
	}
}

// connectToAgent is the ladder roughly half the CLI's commands reach devices
// through (device info/top/shell/logs, bluetooth, audio, os update-status, …).
// It must consult and seed the session broker exactly as resolveTarget does:
// a broker prepared by `wendy run` that `wendy device info` cannot use — and a
// `device info` connect that seeds nothing — leaves the optimization covering
// half the CLI and every one of these commands paying the full post-quantum
// handshake on every invocation.
func TestConnectToAgentConsultsAndSeedsSessionBroker(t *testing.T) {
	stubNonInteractive(t)
	identity := certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}
	setup := func(t *testing.T) {
		writePinTestConfig(t, map[string]config.DevicePin{
			"10.0.0.9": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
		})
		origFlag := deviceFlag
		deviceFlag = "10.0.0.9:50051"
		origConnect, origStart := connectSessionBrokerFn, startSessionBrokerFn
		origLadder, origUpdate := dialAgentLadderFn, checkAndOfferUpdateFn
		origDiscover := discoverLANDevices
		t.Cleanup(func() {
			deviceFlag = origFlag
			connectSessionBrokerFn, startSessionBrokerFn = origConnect, origStart
			dialAgentLadderFn, checkAndOfferUpdateFn = origLadder, origUpdate
			discoverLANDevices = origDiscover
		})
		// These connections are mocked. A real background browse can outlive
		// the test and call another test's dial hook with a LAN device.
		discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) {
			return nil, nil
		}
		checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
			return conn, nil
		}
	}
	sessionConn := func(t *testing.T) *grpcclient.AgentConnection {
		t.Helper()
		conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "10.0.0.9", "10.0.0.9:50052", &config.CertificateInfo{OrganizationID: 7}, identity)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	t.Run("hit uses the broker and skips the dial ladder", func(t *testing.T) {
		setup(t)
		connectSessionBrokerFn = func(ctx context.Context, key string, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
			if key != "10.0.0.9:50051" || expected != identity {
				t.Fatalf("broker consult = (%q, %+v)", key, expected)
			}
			return sessionConn(t), nil
		}
		dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
			t.Error("direct dial ran despite a broker hit")
			return nil, nil, errors.New("unreachable")
		}
		conn, err := connectToAgent(context.Background())
		if err != nil {
			t.Fatalf("connectToAgent: %v", err)
		}
		defer conn.Close()
		if !conn.IsSessionProxy {
			t.Fatal("expected the broker connection to be used")
		}
	})

	t.Run("miss seeds a broker for the verified connection", func(t *testing.T) {
		setup(t)
		connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
			return nil, errors.New("no broker")
		}
		direct := sessionConn(t)
		direct.IsSessionProxy = false // shaped like a fresh verified direct connection
		dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
			return direct, nil, nil
		}
		var seededKey string
		startSessionBrokerFn = func(key string, conn *grpcclient.AgentConnection) error {
			seededKey = key
			if conn != direct {
				t.Error("seeded a different connection than the verified one")
			}
			return nil
		}
		conn, err := connectToAgent(context.Background())
		if err != nil {
			t.Fatalf("connectToAgent: %v", err)
		}
		defer conn.Close()
		if seededKey != "10.0.0.9:50051" {
			t.Fatalf("seeded broker key = %q, want the requested endpoint", seededKey)
		}
	})

	t.Run("DisableSessionBroker forces a fresh transport", func(t *testing.T) {
		setup(t)
		connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
			t.Error("broker consulted despite DisableSessionBroker")
			return nil, errors.New("unreachable")
		}
		startSessionBrokerFn = func(string, *grpcclient.AgentConnection) error {
			t.Error("broker seeded despite DisableSessionBroker")
			return nil
		}
		direct := sessionConn(t)
		direct.IsSessionProxy = false
		dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
			return direct, nil, nil
		}
		conn, err := connectToAgent(context.Background(), DisableSessionBroker())
		if err != nil {
			t.Fatalf("connectToAgent: %v", err)
		}
		conn.Close()
	})
}

func TestConnectPinnedSessionSkipsUnpinnedDevice(t *testing.T) {
	oldLoad, oldConnect := loadConfigForPinFn, connectSessionBrokerFn
	t.Cleanup(func() {
		loadConfigForPinFn = oldLoad
		connectSessionBrokerFn = oldConnect
	})
	loadConfigForPinFn = func() (*config.Config, error) { return &config.Config{}, nil }
	connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		t.Fatal("broker must not be consulted without an exact device pin")
		return nil, nil
	}

	if conn, ok := connectPinnedSession(context.Background(), "new.local:50051"); ok || conn != nil {
		t.Fatalf("connectPinnedSession = (%v, %v), want unavailable", conn, ok)
	}
}
