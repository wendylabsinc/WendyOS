//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestOperatorDirectoryBoundedSignedRowsAndPortRefresh(t *testing.T) {
	r, _, _, mutate := liteManagementFixture(t)
	fixtureLocalOperatorReporter(t, r, mutate, meshoperator.MaxPrincipalBytes)
	initial := r.directorySnapshot(context.Background())
	if initial.State != "ready" || len(initial.Devices) != 1 || initial.Devices[0].Platform != "Lite" || len(initial.Devices[0].Caps) != 2 {
		t.Fatal(initial)
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Announcements[0].AgentPort = 5055; v.Announcements[0].Revision++ })
	changed := r.directorySnapshot(context.Background())
	if changed.Devices[0].Address == initial.Devices[0].Address {
		t.Fatal("signed port did not replace old endpoint")
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Routes[0].Unreachable = true })
	if lost := r.directorySnapshot(context.Background()); lost.State != "ready" || len(lost.Devices) != 0 {
		t.Fatal("lost route remained selectable", lost)
	}
	mutate(func(v *pb.LocalMeshTopology) {
		v.Routes[0].Unreachable = false
		hint, route := v.Announcements[0], v.Routes[0]
		tenant, _ := meshid.ParseTenant(initial.Devices[0].Tenant)
		for i := 0; i < 300; i++ {
			device := meshid.NewDevice(tenant, uuid.New())
			h := proto.Clone(hint).(*pb.LocalMeshAnnouncement)
			h.Principal = device.Principal()
			h.DeviceId = device.UUID().String()
			h.Prefix = device.MeshAddress().String() + "/128"
			h.Caps = nil
			h.Name = ""
			h.Platform = "WendyOS"
			v.Announcements = append(v.Announcements, h)
			p := proto.Clone(route).(*pb.LocalMeshRoute)
			p.Prefix = h.Prefix
			p.OriginPrincipal = h.Principal
			v.Routes = append(v.Routes, p)
		}
	})
	full := r.directorySnapshot(context.Background())
	// The shared watcher adds cadence metadata to the same bounded snapshot.
	full.PollIntervalMillis = operatorDirectoryPoll.Milliseconds()
	full.HeartbeatMillis = operatorDirectoryHeartbeat.Milliseconds()
	encoded, err := json.Marshal(full)
	if err != nil || len(full.Devices) == 0 || len(full.Devices) > operatorDirectoryRows || !full.Truncated || len(encoded) > operatorDirectoryBytes {
		t.Fatal("bounded snapshot", len(full.Devices), len(encoded), full.Truncated, err)
	}
}

func TestOperatorDirectoryWatchCoalescesAndReleasesLease(t *testing.T) {
	r, cc, a, mutate := liteManagementFixture(t)
	source := r.topology
	var polls atomic.Int32
	r.targetTopology = nil // This fixture supplies coherent observations in-process.
	r.topology = func(ctx context.Context) (*pb.LocalMeshTopology, error) { polls.Add(1); return source(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	open := func(parent context.Context) grpc.ClientStream {
		stream, err := cc.NewStream(parent, &grpc.StreamDesc{ServerStreams: true}, operatorWatchMethod, grpc.ForceCodec(rawCodec{}))
		if err != nil {
			t.Fatal(err)
		}
		empty := rawMessage{}
		if err = stream.SendMsg(&empty); err != nil {
			t.Fatal(err)
		}
		stream.CloseSend()
		return stream
	}
	next := func(stream grpc.ClientStream) OperatorDirectory {
		for {
			var bytes rawMessage
			if err := stream.RecvMsg(&bytes); err != nil {
				t.Fatal(err)
			}
			snapshot, err := decodeOperatorDirectory(bytes, nil)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.State == "ready" {
				return snapshot
			}
		}
	}
	firstCtx, closeFirst := context.WithCancel(ctx)
	first, second := open(firstCtx), open(ctx)
	before := next(first)
	_ = next(second)
	if a.activeRPCs.Load() != 2 {
		t.Fatal("watch leases", a.activeRPCs.Load())
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Announcements[0].Name = "fresh name" })
	for next(first).Devices[0].Name != "fresh name" {
	}
	closeFirst()
	// The second consumer did not drain while updates arrived. Its publisher
	// keeps one replaceable snapshot rather than queued historical revisions.
	deadline := time.Now().Add(2 * time.Second)
	for a.activeRPCs.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.activeRPCs.Load() != 1 {
		t.Fatal("canceled watcher retained lease")
	}
	latest := next(second)
	if latest.Devices[0].Principal != before.Devices[0].Principal {
		t.Fatal("identity changed")
	}
	if polls.Load() > 4 {
		t.Fatal("each watcher created a polling loop", polls.Load())
	}
	cancel()
	deadline = time.Now().Add(time.Second)
	for a.activeRPCs.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.activeRPCs.Load() != 0 {
		t.Fatal("watch owner did not release lease")
	}
	r.directory.mu.Lock()
	count := len(r.directory.watchers)
	running := r.directory.cancel != nil
	r.directory.mu.Unlock()
	if count != 0 || running {
		t.Fatal("no-watch idle owner retained work", count, running)
	}
}

func TestOperatorDirectorySlowLocalConsumerAndPendingPollCancel(t *testing.T) {
	r, _, _, mutate := liteManagementFixture(t)
	ch, release, err := r.directory.watch()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mutate(func(v *pb.LocalMeshTopology) { v.Announcements[0].Name = "latest only" })
	time.Sleep(1100 * time.Millisecond)
	if len(ch) != 1 {
		t.Fatal("snapshot queue is not bounded to one", len(ch))
	}
	snapshot := <-ch
	if snapshot.State != "ready" || snapshot.Devices[0].Name != "latest only" {
		t.Fatal(snapshot)
	}
	release()
	entered, ended := make(chan struct{}), make(chan struct{})
	r.targetTopology = nil // This fixture supplies coherent observations in-process.
	r.topology = func(ctx context.Context) (*pb.LocalMeshTopology, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	}
	_, release, err = r.directory.watch()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	release()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("last watcher did not cancel pending poll")
	}
}

func TestOperatorDirectoryUnchangedCaptureSuppressionAndHeartbeat(t *testing.T) {
	r, _, _, mutate := liteManagementFixture(t)
	snapshot := r.directorySnapshot(context.Background())
	snapshot.PollIntervalMillis = operatorDirectoryPoll.Milliseconds()
	snapshot.HeartbeatMillis = operatorDirectoryHeartbeat.Milliseconds()
	var publication directoryPublication
	now := time.Now()
	if !publication.shouldEmit(snapshot, now) {
		t.Fatal("first snapshot not emitted")
	}
	snapshot.Captured++
	snapshot.SourceResponseBytes++
	if publication.shouldEmit(snapshot, now.Add(time.Second)) {
		t.Fatal("timestamp/diagnostic-size-only change emitted payload")
	}
	if !publication.shouldEmit(snapshot, now.Add(operatorDirectoryHeartbeat)) {
		t.Fatal("explicit heartbeat did not refresh healthy unchanged view")
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Routes[0].Unreachable = true })
	lost := r.directorySnapshot(context.Background())
	lost.PollIntervalMillis = snapshot.PollIntervalMillis
	lost.HeartbeatMillis = snapshot.HeartbeatMillis
	if !publication.shouldEmit(lost, now.Add(operatorDirectoryHeartbeat+time.Millisecond)) {
		t.Fatal("route withdrawal waited for heartbeat")
	}
	lost.State = "unavailable"
	if !publication.shouldEmit(lost, now.Add(operatorDirectoryHeartbeat+2*time.Millisecond)) {
		t.Fatal("source loss waited for heartbeat")
	}
	encoded, _ := json.Marshal(snapshot)
	t.Logf("actual normal fixture: source protobuf response=%d bytes; DirectoryV1 JSON=%d bytes; shared backend poll=%s; unchanged IPC heartbeat=%s", snapshot.SourceResponseBytes-1, len(encoded), operatorDirectoryPoll, operatorDirectoryHeartbeat)
}

func TestOperatorDirectoryWatchSuppressesTimestampOnlyPollsAndWithdrawsPromptly(t *testing.T) {
	r, _, _, mutate := liteManagementFixture(t)
	ch, release, err := r.directory.watch()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.After(2 * time.Second)
	var ready OperatorDirectory
	for ready.State != "ready" {
		select {
		case ready = <-ch:
		case <-deadline:
			t.Fatal("first ready snapshot did not arrive")
		}
	}
	if ready.PollIntervalMillis != 1000 || ready.HeartbeatMillis != 5000 || ready.SourceResponseBytes == 0 {
		t.Fatal("watch traffic metadata", ready)
	}
	time.Sleep(1100 * time.Millisecond)
	if len(ch) != 0 {
		t.Fatal("unchanged capture timestamp produced another payload")
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Routes[0].Unreachable = true })
	select {
	case withdrawn := <-ch:
		if len(withdrawn.Devices) != 0 {
			t.Fatal("route remained selectable", withdrawn)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("route withdrawal waited for the five-second heartbeat")
	}
}

func TestOperatorDirectoryNormalHandoverChangesSourceAndKeepsTarget(t *testing.T) {
	r, cc, _, mutate := liteManagementFixture(t)
	_, original, target := remoteTopologyFixture(t)
	peer := original
	r.currentAdjacent = func() (*grpc.ClientConn, meshid.Device, error) { return cc, peer, nil }
	initial := r.directorySnapshot(context.Background())
	if initial.Source != original.Principal() || len(initial.Devices) != 1 {
		t.Fatal(initial)
	}
	replacement, _ := meshid.ParseDevice(original.Tenant(), "44444444-4444-4444-8444-444444444444")
	peer = replacement
	mutate(func(view *pb.LocalMeshTopology) {
		view.SelfPrincipal = replacement.Principal()
		view.SelfDeviceId = replacement.UUID().String()
		view.SelfMeshAddress = replacement.MeshAddress().String()
		view.Routes[0].NextHopPrincipal = replacement.Principal()
	})
	changed := r.directorySnapshot(context.Background())
	if changed.State != "ready" || changed.Source != replacement.Principal() || len(changed.Devices) != 1 || changed.Devices[0].Principal != target.Principal() || changed.Devices[0].NextHop != replacement.Principal() {
		t.Fatal("handover directory confused first hop with selected target", changed)
	}
	// Default IPC RPCs still name the originally selected device; a new first
	// hop must never silently substitute its own management interface.
	mutate(func(view *pb.LocalMeshTopology) { view.Announcements = nil; view.Routes = nil })
	if selected, release, err := r.Resolve(context.Background()); err == nil {
		release()
		t.Fatal("old target silently became replacement peer", selected)
	}
}

// Exercise the production local-node shape rather than a first-hop observer.
func fixtureLocalOperatorReporter(t *testing.T, r *operatorTargets, mutate func(func(*pb.LocalMeshTopology)), principalBytes int) {
	t.Helper()
	op, err := operatorPrincipal(r.certificate)
	if err != nil {
		t.Fatal(err)
	}
	principal := op.Principal()
	if principalBytes > len(principal) {
		prefix := "spiffe://wendy.sh/tenant/" + op.Tenant().String() + "/operator/"
		principal = prefix + strings.Repeat("s", principalBytes-len(prefix))
		certificate, _ := liteManagementCertificate(t, nil, nil, "operator", principal, false)
		r.certificate = config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))}
	}
	self, err := meshid.PeerFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	self, err = self.WithInstance(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	r.localSnapshot = func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{Self: self} }
	mutate(func(v *pb.LocalMeshTopology) {
		v.SelfPrincipal = self.Principal()
		v.SelfTenantId = self.Tenant().String()
		v.SelfDeviceId = self.UUID().String()
		v.SelfMeshAddress = self.MeshAddress().String()
	})
}
