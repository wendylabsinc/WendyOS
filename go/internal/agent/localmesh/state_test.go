package localmesh

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestStateStoreRestartAndIdentityBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	id, err := instanceRouterID()
	if err != nil {
		t.Fatal(err)
	}
	s, state, err := OpenStateStore(path, device, id)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := babel.New(babel.Config{RouterID: id})
	cp := engine.Checkpoint()
	state.Revision = 123
	state.Babel = &cp
	if err = s.Save(state); err != nil {
		t.Fatal(err)
	}
	_, restored, err := OpenStateStore(path, device, id)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Revision != 123 || restored.Babel.RouterID != id {
		t.Fatal(restored)
	}
	// The same device UUID under another tenant is a different instance.
	if _, _, err = OpenStateStore(path, testDevice(t, testPrincipal(testOtherTenant, testDeviceAID)), id); err == nil {
		t.Fatal("cross-tenant safety state reused")
	}
}

func TestDeviceStateStoreLegacyEnrollmentAndPrincipalIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing-state.json")
	// Shape read from the failing Thor after its legacy unenrollment and new
	// enrollment. Missing device/routerID fields must not inherit the new ones.
	legacy := []byte(`{"Version":1,"Org":64,"Asset":460,"Revision":101180,"Babel":{"Version":1,"RouterID":274877907404,"Seqno":897},"Receipts":[{"Asset":460,"Revision":101180}]}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	first := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	other := testDevice(t, testPrincipal(testOtherTenant, testDeviceAID))
	id := babel.RouterIDFromRandom([6]byte{1, 2, 3, 4, 5, 6})
	store, state, err := OpenDeviceStateStore(path, first, id)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(store.path) != filepath.Dir(path) {
		t.Fatal("principal state introduced an ancestor outside the fsynced directory", store.path)
	}
	if state.Device != first.Principal() || state.RouterID != uint64(id) || state.Revision != 0 || state.Babel != nil || len(state.Receipts) != 0 {
		t.Fatalf("legacy state leaked into the new principal: %+v", state)
	}
	engine, err := babel.New(babel.Config{RouterID: id, InitialSeqno: 901})
	if err != nil {
		t.Fatal(err)
	}
	cp := engine.Checkpoint()
	state.Revision, state.Babel = 123, &cp
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	otherStore, otherState, err := OpenDeviceStateStore(path, other, id)
	if err != nil {
		t.Fatal(err)
	}
	if otherStore.path == store.path || otherState.Revision != 0 || otherState.Babel != nil {
		t.Fatal("another tenant inherited safety state", otherState)
	}
	otherState.Revision = 456
	if err := otherStore.Save(otherState); err != nil {
		t.Fatal(err)
	}
	// Returning to the first full principal resumes its revision and router,
	// even if hardware/random selection now supplies another candidate.
	_, restored, err := OpenDeviceStateStore(path, first, id+1)
	if err != nil || !reflect.DeepEqual(restored, state) {
		t.Fatalf("returning identity lost safety history: %+v %v", restored, err)
	}
	if _, err := babel.Restore(babel.Config{RouterID: babel.RouterID(restored.RouterID)}, *restored.Babel); err != nil {
		t.Fatalf("restored checkpoint is unusable: %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, legacy) {
		t.Fatal("legacy rollback state was changed", err)
	}
}

func TestDeviceStateStoreImportsCurrentPrincipalWithoutResettingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing-state.json")
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	id := babel.RouterIDFromRandom([6]byte{6, 5, 4, 3, 2, 1})
	unscoped, state, err := OpenStateStore(path, device, id)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := babel.New(babel.Config{RouterID: id, InitialSeqno: 64000})
	if err != nil {
		t.Fatal(err)
	}
	cp := engine.Checkpoint()
	state.Revision, state.Babel = 567, &cp
	if err := unscoped.Save(state); err != nil {
		t.Fatal(err)
	}
	store, restored, err := OpenDeviceStateStore(path, device, id+1)
	if err != nil || store.path == path || !reflect.DeepEqual(restored, state) {
		t.Fatalf("migration reset safety history: %+v %v", restored, err)
	}
	if _, err := os.Stat(store.path); err != nil {
		t.Fatal("migration was not persisted before returning", err)
	}
	// Once imported, later starts must use the newer scoped checkpoint rather
	// than re-importing the stale unscoped file after each restart.
	restored.Revision++
	if err := store.Save(restored); err != nil {
		t.Fatal(err)
	}
	_, resumed, err := OpenDeviceStateStore(path, device, id+2)
	if err != nil || !reflect.DeepEqual(resumed, restored) {
		t.Fatalf("restart imported stale state: %+v %v", resumed, err)
	}
}

func TestDeviceStateStoreDoesNotResetMalformedCurrentState(t *testing.T) {
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	id := babel.RouterIDFromRandom([6]byte{1, 2, 3, 4, 5, 6})
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unscoped", true: "scoped"}[scoped], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routing-state.json")
			open := OpenStateStore
			if scoped {
				open = OpenDeviceStateStore
			}
			store, state, err := open(path, device, id)
			if err != nil {
				t.Fatal(err)
			}
			state.Babel = &babel.Checkpoint{Version: 1, RouterID: id + 1, Seqno: 100}
			if err := store.Save(state); err != nil {
				t.Fatal(err)
			}
			if _, _, err := OpenDeviceStateStore(path, device, id); err == nil {
				t.Fatal("corrupt current identity was reset instead of refused")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "routing-state.json")
	if err := os.WriteFile(path, []byte(`{"Version":1,"revision":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenStateStore(path, device, id); err == nil {
		t.Fatal("missing disk identity was filled from current credentials")
	}
	if _, _, err := OpenDeviceStateStore(path, device, id); err == nil {
		t.Fatal("malformed current state was mistaken for a legacy identity")
	}
}

// A new candidate router ID must not invalidate a checkpoint for the same device.
func TestStateStoreRetainsRouterIDAcrossHardwareOrRandomChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	id, err := instanceRouterID()
	if err != nil {
		t.Fatal(err)
	}
	s, state, err := OpenStateStore(path, device, id)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := babel.New(babel.Config{RouterID: id})
	cp := engine.Checkpoint()
	state.Babel = &cp
	if err = s.Save(state); err != nil {
		t.Fatal(err)
	}
	other, err := babel.RouterIDFromMAC(net.HardwareAddr{0x02, 0, 0, 0, 0, 0x63})
	if err != nil {
		t.Fatal(err)
	}
	if _, restored, err := OpenStateStore(path, device, other); err != nil || restored.RouterID != uint64(id) || restored.Babel.RouterID != id {
		t.Fatalf("saved routing identity was not retained: %+v %v", restored, err)
	}
	// A checkpoint whose own router ID disagrees with the file's pin is refused
	// even when the pin matches this instance.
	if _, _, err = OpenStateStore(path, device, id); err != nil {
		t.Fatal(err)
	}
	tampered := state
	tampered.Babel = &babel.Checkpoint{RouterID: other, Seqno: cp.Seqno}
	if err = s.Save(tampered); err != nil {
		t.Fatal(err)
	}
	if _, _, err = OpenStateStore(path, device, id); err == nil {
		t.Fatal("checkpoint from another router was resumed")
	}
}
