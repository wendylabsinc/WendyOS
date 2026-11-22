package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func newTestGenericSensorFeed(t testing.TB) *GenericSensorFeed {
	t.Helper()
	return NewGenericSensorFeed(zap.NewNop(), filepath.Join(t.TempDir(), "channel-ids"))
}

func TestGenericSensorChannelIDsRejectOldSubscriptionAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "channel-ids")
	descriptors := []*sensorpb.SensorDescriptor{{
		ChannelId: 7,
		Format:    &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example", SampleBytes: 4}},
	}}
	first := NewGenericSensorFeed(zap.NewNop(), path)
	_, _, releaseFirst := first.Mount(mcusource.SensorPairing{SourceAssetID: 1}, descriptors)
	manifest, _ := first.GetSensorManifest(context.Background(), nil)
	releaseFirst()
	if len(manifest.GetSensors()) != 1 {
		t.Fatalf("first manifest = %v", manifest)
	}
	oldID := manifest.Sensors[0].GetChannelId()

	restarted := NewGenericSensorFeed(zap.NewNop(), path)
	_, _, releaseRestarted := restarted.Mount(mcusource.SensorPairing{SourceAssetID: 2}, descriptors)
	defer releaseRestarted()
	manifest, _ = restarted.GetSensorManifest(context.Background(), nil)
	if len(manifest.GetSensors()) != 1 || manifest.Sensors[0].GetChannelId() != 33 {
		t.Fatalf("first channel after restart should skip the reserved block: %v", manifest)
	}
	err := restarted.StreamSensors(&agentpb.StreamSensorsRequest{ChannelId: []uint32{oldID}}, nil)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("old channel %d returned %v, want NotFound", oldID, err)
	}
}

func TestGenericChannelIDsReserveBlocks(t *testing.T) {
	ids := genericChannelIDs{path: filepath.Join(t.TempDir(), "channel-ids")}
	for want := uint32(1); want <= 33; want++ {
		got, err := ids.allocate()
		if err != nil || got != want {
			t.Fatalf("allocate() = %d, %v, want %d", got, err, want)
		}
		high, err := os.ReadFile(ids.path)
		wantHigh := "32"
		if want == 33 {
			wantHigh = "64"
		}
		if err != nil || string(high) != wantHigh {
			t.Fatalf("counter after allocating %d = %q, %v, want %q", want, high, err, wantHigh)
		}
	}
}

func TestGenericChannelIDsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channel-ids")
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := genericChannelIDs{path: path}
	if got, err := ids.allocate(); err == nil || got != 0 {
		t.Fatalf("allocated %d with invalid counter, err=%v", got, err)
	}
	if err := os.WriteFile(path, strconv.AppendUint(nil, math.MaxUint32-1, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ids.allocate(); err != nil || got != math.MaxUint32 {
		t.Fatalf("last ID = %d, %v, want %d", got, err, uint32(math.MaxUint32))
	}
	if got, err := ids.allocate(); err == nil || got != 0 {
		t.Fatalf("allocated %d after exhaustion, err=%v", got, err)
	}
	restarted := genericChannelIDs{path: path}
	if got, err := restarted.allocate(); err == nil || got != 0 {
		t.Fatalf("allocated %d after restart with exhausted counter, err=%v", got, err)
	}
}

func TestGenericSensorFeedIsolationLifetimeAndBackpressure(t *testing.T) {
	s := newTestGenericSensorFeed(t)
	descriptors := []*sensorpb.SensorDescriptor{{ChannelId: 1, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example", SampleBytes: 2}}}}
	_, pub1, close1 := s.Mount(mcusource.SensorPairing{SourceAssetID: 1}, descriptors)
	_, pub2, close2 := s.Mount(mcusource.SensorPairing{SourceAssetID: 2}, descriptors)
	defer close2()
	m, _ := s.GetSensorManifest(context.Background(), &agentpb.GetSensorManifestRequest{})
	if len(m.Sensors) != 2 || m.Sensors[0].ChannelId == m.Sensors[1].ChannelId || m.Sensors[0].GetSource().GetPairingKey() == m.Sensors[1].GetSource().GetPairingKey() {
		t.Fatalf("source collision: %v", m)
	}
	id1, id2 := m.Sensors[0].ChannelId, m.Sensors[1].ChannelId
	sub := &genericSubscription{ids: map[uint32]bool{id1: true}, frames: make(chan *sensorpb.SensorData, 8), done: make(chan struct{})}
	s.subscribers[sub] = true
	frame := &sensorlink.SensorFrame{ChannelID: 1, Seq: 7, TsUs: 100, Payload: []byte{1, 2}}
	if !pub2(frame) || len(sub.frames) != 0 {
		t.Fatal("leaked another source")
	}
	if pub1(&sensorlink.SensorFrame{ChannelID: 1, TsUs: 100, Payload: []byte{1}}) {
		t.Fatal("accepted malformed sample")
	}
	pub1(frame)
	got := <-sub.frames
	frame.Payload[0] = 9
	if got.ChannelId != id1 || got.Payload[0] != 1 || got.Flags&2 == 0 || got.FrameSeq != 7 {
		t.Fatalf("bad copy: %v", got)
	}
	close1()
	close1()
	select {
	case <-sub.done:
	default:
		t.Fatal("disconnect did not end subscription")
	}
	if pub1(frame) {
		t.Fatal("publishing after unmount")
	}
	m, _ = s.GetSensorManifest(context.Background(), &agentpb.GetSensorManifestRequest{})
	if len(m.Sensors) != 1 || m.Sensors[0].ChannelId != id2 {
		t.Fatal("unmount affected wrong source")
	}
	slow := &genericSubscription{ids: map[uint32]bool{id2: true}, frames: make(chan *sensorpb.SensorData, 8), done: make(chan struct{})}
	s.subscribers[slow] = true
	for i := 0; i < 10; i++ {
		frame.TsUs++
		pub2(frame)
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("slow reader not disconnected")
	}
	if len(slow.frames) > 8 {
		t.Fatal("unbounded queue")
	}
}

func BenchmarkGenericSensorFeedWithoutReaders(b *testing.B) {
	s := newTestGenericSensorFeed(b)
	_, pub, release := s.Mount(mcusource.SensorPairing{SourceAssetID: 1}, []*sensorpb.SensorDescriptor{{ChannelId: 1, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example.array.v1", SampleBytes: 1536}}}})
	defer release()
	f := &sensorlink.SensorFrame{ChannelID: 1, TsUs: 1, Payload: make([]byte, 1536)}
	pub(f)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.TsUs++
		pub(f)
	}
}

func TestGenericManifestPreservesFormatsAndIdentityAcrossReconnect(t *testing.T) {
	feed := newTestGenericSensorFeed(t)
	descriptors := []*sensorpb.SensorDescriptor{
		{ChannelId: 0, InputId: 7, Name: "温度", Source: &sensorpb.SensorSource{PairingKey: "spoofed", ChannelId: 99, InputId: 99}, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "vendor.unknown.v9", RateHz: 0, SampleBytes: 2}}},
		{ChannelId: 9, InputId: 7, Name: "alternative", Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "vendor.other", SampleBytes: 4}}},
	}
	ids, pub, release := feed.Mount(mcusource.SensorPairing{SourceAssetID: 3}, descriptors)
	if len(ids) != 2 || ids[0] != 0 {
		t.Fatal(ids)
	}
	manifest, _ := feed.GetSensorManifest(context.Background(), nil)
	first, second := manifest.Sensors[0], manifest.Sensors[1]
	if first.Name != "温度" || first.GetSensor().Schema != "vendor.unknown.v9" || first.InputId != second.InputId {
		t.Fatal(manifest)
	}
	source := proto.Clone(first.GetSource()).(*sensorpb.SensorSource)
	if source.GetPairingKey() != "asset:3" || source.GetChannelId() != 0 || source.GetInputId() != 7 {
		t.Fatalf("untrusted source identity: %v", source)
	}
	first.Source.PairingKey = "mutated client"
	descriptors[0].Name = "mutated upstream"
	first.Name = "mutated client"
	refreshed, _ := feed.GetSensorManifest(context.Background(), nil)
	if refreshed.Sensors[0].Name != "温度" || !proto.Equal(refreshed.Sensors[0].Source, source) {
		t.Fatal("manifest aliases mutable descriptors")
	}
	// Zero, equal and reset timestamps are legitimate opaque source timestamps.
	for _, ts := range []uint64{0, 100, 100, 1} {
		if !pub(&sensorlink.SensorFrame{ChannelID: 0, TsUs: ts, Payload: []byte{0, 1}}) {
			t.Fatal("timestamp interpreted by forwarding layer")
		}
	}
	release()
	_, _, release = feed.Mount(mcusource.SensorPairing{SourceAssetID: 3}, descriptors)
	defer release()
	refreshed, _ = feed.GetSensorManifest(context.Background(), nil)
	if refreshed.Sensors[0].ChannelId == first.ChannelId || !proto.Equal(refreshed.Sensors[0].GetSource(), source) {
		t.Fatal("reconnect lost source identity or reused old routing ID")
	}
}

func TestGenericFeedBoundsAndRejectedChannels(t *testing.T) {
	feed := newTestGenericSensorFeed(t)
	descriptors := make([]*sensorpb.SensorDescriptor, 1025)
	for i := range descriptors {
		descriptors[i] = &sensorpb.SensorDescriptor{ChannelId: uint32(i), Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "unknown", SampleBytes: 4}}}
	}
	accepted, pub, release := feed.Mount(mcusource.SensorPairing{SourceAssetID: 1}, descriptors)
	defer release()
	if len(accepted) != 1024 {
		t.Fatal(len(accepted))
	}
	if pub(&sensorlink.SensorFrame{ChannelID: 1024, Payload: []byte{1, 2, 3, 4}}) {
		t.Fatal("published rejected channel")
	}
	for _, tc := range []struct {
		size int
		want bool
	}{
		{0, false},
		{3, false},
		{4, true},
		{5, false},
		{6000, true},
		{65536, true},
		{65540, false},
	} {
		if got := pub(&sensorlink.SensorFrame{ChannelID: 1, Payload: make([]byte, tc.size)}); got != tc.want {
			t.Errorf("%d-byte frame accepted=%v, want %v", tc.size, got, tc.want)
		}
	}
}
