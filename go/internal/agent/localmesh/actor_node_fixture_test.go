package localmesh

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Simulated devices share the test host's permanent MAC on Linux. Seed each
// device's persisted routing identity separately, just as independent hardware
// would, while keeping real Node, Directory and Babel behavior in the fixture.
func seedActorTestIdentity(t *testing.T, dir string, credentials *Credentials) {
	t.Helper()
	// This deterministic mapping is test-only; production keeps its hardware
	// identity policy. Restarting a fixture retains the existing checkpoint.
	digest := sha256.Sum256([]byte(credentials.Device.Key()))
	var random [6]byte
	copy(random[:], digest[:])
	routerID := babel.RouterIDFromRandom(random)
	store, state, err := OpenDeviceStateStore(filepath.Join(dir, "routing-state.json"), credentials.Device, routerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
}

func newActorTestNode(t *testing.T, credentials *Credentials, network PacketNetwork) *Node {
	t.Helper()
	dir := t.TempDir()
	seedActorTestIdentity(t, dir, credentials)
	n, err := NewNodeWithNetwork(context.Background(), dir, credentials, "actor-test", 50052, network)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
