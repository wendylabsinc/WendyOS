package localmesh

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Exercise the actual constructor, signed origin publication and checkpoint
// reopen, rather than only the random-ID helper. The permanent-MAC CI fixture
// gives every Node the same physical host identity.
func TestOperatorNodeRouterIdentitiesAreInstallationScoped(t *testing.T) {
	device, _ := testCredentials(t)
	physicalID, _ := publishedNodeIdentity(t, t.TempDir(), device)
	if mac, ok := permanentMAC(); ok {
		want, err := babel.RouterIDFromMAC(mac)
		if err != nil || physicalID != want {
			t.Fatalf("device lost its permanent-MAC identity: got %x want %x: %v", physicalID, want, err)
		}
	}
	seen := map[babel.RouterID]bool{physicalID: true}
	principal := "spiffe://wendy.sh/tenant/" + device.Device.Tenant().String() + "/operator/same-host"
	for _, instance := range []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"} {
		// Separate valid leaf keys model two ordinary logins/installations of
		// the same operator, not reuse of an enrolled device identity.
		cert, key := testIssue(t, principal)
		credentials, err := NewOperatorCredentials(cert, testPKI().chainPEM, key, uuid.MustParse(instance))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		id, revision := publishedNodeIdentity(t, dir, credentials)
		if id>>48 != 1 || seen[id] {
			t.Fatalf("operator installation shared another router identity: %x", id)
		}
		seen[id] = true
		again, nextRevision := publishedNodeIdentity(t, dir, credentials)
		if again != id || nextRevision <= revision {
			t.Fatalf("restart lost pinned identity/history: %x/%d -> %x/%d", id, revision, again, nextRevision)
		}
		t.Logf("installation %s retained router %x across restart", instance, id)
	}
}

func TestOperatorNodeRetainsExistingPinnedRouterIdentity(t *testing.T) {
	device, _ := testCredentials(t)
	cert, key := testIssue(t, "spiffe://wendy.sh/tenant/"+device.Device.Tenant().String()+"/operator/existing-install")
	credentials, err := NewOperatorCredentials(cert, testPKI().chainPEM, key, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pinned, err := babel.RouterIDFromMAC([]byte{0x0a, 0xff, 0xec, 0x0f, 0x81, 0x71})
	if err != nil {
		t.Fatal(err)
	}
	store, state, err := OpenDeviceStateStore(filepath.Join(dir, "routing-state.json"), credentials.Device, pinned)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := babel.New(babel.Config{RouterID: pinned, InitialSeqno: 77})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := engine.Checkpoint()
	state.Babel, state.Revision = &checkpoint, 123
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	id, revision := publishedNodeIdentity(t, dir, credentials)
	if id != pinned || revision <= state.Revision {
		t.Fatalf("existing routing identity/history was reset: %x/%d", id, revision)
	}
}

func publishedNodeIdentity(t *testing.T, dir string, credentials *Credentials) (babel.RouterID, uint64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node, err := NewNodeWithNetwork(ctx, dir, credentials, "router identity fixture", 50052, &lifecyclePacketNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- node.Run() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("identity fixture Node did not join shutdown")
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		view := node.Snapshot()
		if len(view.Devices) == 1 {
			manifest := view.Devices[0]
			if manifest.Route == nil || !view.RouteAuthorizations[*manifest.Route] {
				t.Fatal("Node did not publish an authenticated own origin")
			}
			if credentials.Device.IsOperator() && (manifest.Owner == nil || manifest.Owner.Instance != credentials.Device.UUID()) {
				t.Fatal("origin lost its actual operator installation owner")
			}
			return manifest.Route.RouterID, manifest.Revision
		}
		select {
		case <-deadline.C:
			t.Fatal("Node did not publish its own signed origin")
		case <-time.After(time.Millisecond):
		}
	}
}
