package localmesh

import (
	"context"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNodePublicationUsesLocalCertificateClockWithoutMovingLeaseClock(t *testing.T) {
	for _, skew := range []time.Duration{-598 * time.Second, 598 * time.Second} {
		t.Run(skew.String(), func(t *testing.T) {
			node := routePublicationNode(t)
			wall := time.Now().Add(skew).Truncate(time.Second)
			node.Credentials.timePolicy = &certs.CertificateTimePolicy{Now: func() time.Time { return wall }}
			before := time.Now()
			if err := node.Persist(node.routing.Checkpoint()); err != nil {
				t.Fatal(err)
			}
			signed := node.advertisement
			if signed.Issued != wall.UnixMilli() {
				t.Fatalf("logical publication did not follow local policy: %v want %v", time.UnixMilli(signed.Issued), wall)
			}
			remaining := signed.LeaseRemaining(time.Now())
			if remaining <= MaxLease-time.Second || remaining > MaxLease {
				t.Fatalf("certificate clock shifted admitted TTL: %v", remaining)
			}
			scheduled := node.publicationDeadline()
			if scheduled.Before(before.Add(5*time.Minute-time.Second)) || scheduled.After(time.Now().Add(5*time.Minute)) {
				t.Fatalf("publication schedule used virtual UTC: %v", scheduled)
			}
			// Policy UTC can move in either direction while an unexpired local edit
			// gate and renewal timer keep their original monotonic deadlines.
			wall = wall.Add(-2 * skew)
			if !node.publicationDeadline().Equal(scheduled) {
				t.Fatal("UTC correction moved renewal deadline")
			}
			if !node.advertiseLocalRoute(signed.Route.RouterID, signed.Route.Prefix, signed.Route.Seqno) {
				t.Fatal("virtual clock movement removed the live local origin")
			}
		})
	}
}

func TestNodeMigratesOnlyObsoleteForeignReceiveReceipts(t *testing.T) {
	own, peer := testCredentials(t)
	dir := t.TempDir()
	router, err := routerIDCandidate(own.Device)
	if err != nil {
		t.Fatal(err)
	}
	store, state, err := OpenDeviceStateStore(filepath.Join(dir, "routing-state.json"), own.Device, router)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := babel.New(babel.Config{RouterID: router, InitialSeqno: 77})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := engine.Checkpoint()
	state.Babel = &checkpoint
	state.Revision = 123
	state.Receipts = []DirectoryReceipt{{Device: peer.Device.Key(), Revision: 98, Hash: strings.Repeat("0", 64), Until: time.Now().Add(time.Hour)}}
	if err = store.Save(state); err != nil {
		t.Fatal(err)
	}
	node, err := NewNodeWithNetwork(context.Background(), dir, own, "migration", 50052, &lifecyclePacketNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	defer node.cancel()
	defer node.kernel.Close()
	if node.state.Revision != 123 || node.state.RouterID != state.RouterID || !reflect.DeepEqual(node.state.Babel, state.Babel) {
		t.Fatal("receipt migration changed origin/Babel safety state")
	}
	if len(node.state.Receipts) != 0 || len(node.directory.history) != 0 || len(node.directory.Snapshot(time.Now())) != 0 {
		t.Fatal("legacy foreign receipt restored or retained as current lease")
	}
	// Construction validates old data, then the next ordinary local checkpoint
	// removes only the abandoned receive-receipt field from the file.
	if err = node.store.Save(node.state); err != nil {
		t.Fatal(err)
	}
	restored, _, err := readPersistentState(node.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Receipts) != 0 || restored.Revision != 123 || restored.RouterID != state.RouterID || !reflect.DeepEqual(restored.Babel, state.Babel) {
		t.Fatal("normal save changed unrelated persisted fields")
	}
}
