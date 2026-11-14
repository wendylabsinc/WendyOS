package localmesh

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
)

// ClockRuntimeFixture is exported only in the test variant so external
// composition tests can use the production netmesh packet backend without an
// import cycle. These are the existing BootClock/Directory clock seams; no
// production time or network behavior is replaced.
type ClockRuntimeFixture struct {
	Clock   *BootClock
	start   time.Time
	wall    atomic.Int64
	elapsed atomic.Int64
}

func (f *ClockRuntimeFixture) SetWall(now time.Time) {
	f.wall.Store(now.UnixNano() - time.Since(f.start).Nanoseconds())
}
func (f *ClockRuntimeFixture) AdvanceElapsed(elapsed time.Duration) { f.elapsed.Add(int64(elapsed)) }
func (f *ClockRuntimeFixture) monotonicNow() time.Time {
	return time.Now().Add(time.Duration(f.elapsed.Load()))
}

func OpenClockRuntimeFixture(t *testing.T, directory string, ordinal int, anchor, wall time.Time) (*ClockRuntimeFixture, *Credentials) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	leafPath := filepath.Join(directory, "provisioned.pem")
	keyPath := filepath.Join(directory, "provisioned.key")
	savedPath := filepath.Join(directory, "saved-clock")
	if _, err := os.Stat(leafPath); os.IsNotExist(err) {
		device := testDeviceAID
		if ordinal == 1 {
			device = testDeviceBID
		}
		leaf, key := issuePolicyLeaf(t, testPrincipal(testTenantUUID, device), anchor.Add(-time.Hour), anchor.Add(24*time.Hour))
		for path, data := range map[string]string{leafPath: leaf, keyPath: key, savedPath: "local saved-clock fixture"} {
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := timesync.WriteFloor(directory, anchor.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(savedPath, anchor, anchor); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	fixture := &ClockRuntimeFixture{start: time.Now()}
	fixture.SetWall(wall)
	clock, err := newBootClock(directory, filepath.Join(directory, "absent-fallback"), savedPath,
		func() time.Time { return time.Unix(0, fixture.wall.Load()).Add(time.Since(fixture.start)) },
		func() (int64, error) { return time.Since(fixture.start).Nanoseconds() + fixture.elapsed.Load(), nil })
	if err != nil {
		t.Fatal(err)
	}
	fixture.Clock = clock
	leaf, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := clock.AddProvisionedCertificate(string(leaf)); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentialsWithTimePolicy(string(leaf), testPKI().chainPEM, string(key), clock.Policy())
	if err != nil {
		t.Fatal(err)
	}
	return fixture, credentials
}

func NewClockRuntimeNode(t *testing.T, ctx context.Context, directory string, credentials *Credentials, network PacketNetwork, fixture *ClockRuntimeFixture) *Node {
	t.Helper()
	seedActorTestIdentity(t, directory, credentials)
	node, err := NewNodeWithNetwork(ctx, directory, credentials, "clock-packet-regression", 50052, network)
	if err != nil {
		t.Fatal(err)
	}
	// Assigned before Run and before any carrier can admit a foreign record.
	node.directory.monotonicNow = fixture.monotonicNow
	return node
}

func ClockRuntimeRevision(t *testing.T, node *Node) uint64 {
	t.Helper()
	state, _, err := readPersistentState(node.store.path)
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}
