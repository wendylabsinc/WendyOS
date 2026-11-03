package localmesh

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Simulated devices share the test host's permanent MAC on Linux. Seed each
// device's persisted routing identity separately, just as independent hardware
// would, while keeping real Node, Directory and Babel behavior in the fixture.
func newActorTestNode(t *testing.T, credentials *Credentials, ordinal byte, network PacketNetwork) *Node {
	t.Helper()
	dir := t.TempDir()
	routerID := babel.RouterIDFromRandom([6]byte{0, 0, 0, 0, 0, ordinal + 1})
	store, state, err := OpenDeviceStateStore(filepath.Join(dir, "routing-state.json"), credentials.Device, routerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	n, err := NewNodeWithNetwork(context.Background(), dir, credentials, "actor-test", 50052, network)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
