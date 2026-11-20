package services

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// GenericSensorFeed exposes paired, authenticated generic samples on the local
// read-only socket. It never handles device keys or decodes application schemas.
// Channel and input IDs are session-local. Each descriptor's typed source maps
// these IDs back to the paired source and its native IDs. No history is replayed.
type GenericSensorFeed struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	mu          sync.Mutex
	next        uint32
	channels    map[uint32]*sensorlinkpb.SensorDescriptor
	subscribers map[*genericSubscription]bool
}

type genericSubscription struct {
	ids    map[uint32]bool
	frames chan *sensorlinkpb.SensorData
	done   chan struct{}
	err    error
}

func NewGenericSensorFeed() *GenericSensorFeed {
	return &GenericSensorFeed{channels: make(map[uint32]*sensorlinkpb.SensorDescriptor), subscribers: make(map[*genericSubscription]bool)}
}

// NewSensorReadServer deliberately excludes all control-plane RPCs and reflection.
func NewSensorReadServer(feed *GenericSensorFeed) *grpc.Server {
	s := grpc.NewServer(grpc.MaxRecvMsgSize(4096), grpc.MaxConcurrentStreams(16))
	agentpbv2.RegisterWendySensorServiceServer(s, feed)
	return s
}

// Mount assigns distinct IDs even when different sources use the same channel.
func (s *GenericSensorFeed) Mount(p mcusource.SensorPairing, descriptors []*sensorlinkpb.SensorDescriptor) ([]uint32, func(*sensorlink.SensorFrame) bool, func()) {
	s.mu.Lock()
	ids := make(map[uint32]uint32)
	inputs := make(map[uint32]uint32)
	var accepted []uint32
	for _, d := range descriptors {
		if !mcusource.ValidGenericDescriptor(d) || ids[d.ChannelId] != 0 {
			continue
		}
		if len(s.channels) >= 1024 || s.next == math.MaxUint32 {
			break
		}
		s.next++
		id := s.next
		ids[d.ChannelId] = id
		clone := proto.Clone(d).(*sensorlinkpb.SensorDescriptor)
		clone.ChannelId = id
		if _, ok := inputs[d.InputId]; !ok {
			inputs[d.InputId] = id
		}
		clone.InputId = inputs[d.InputId]
		// Identity comes from the authenticated pairing, never the source payload.
		clone.Source = &sensorlinkpb.SensorSource{PairingKey: p.Key(), ChannelId: d.ChannelId, InputId: d.InputId}
		accepted = append(accepted, d.ChannelId)
		s.channels[id] = clone
	}
	s.mu.Unlock()
	active := true
	publish := func(f *sensorlink.SensorFrame) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !active || f == nil {
			return false
		}
		id := ids[f.ChannelID]
		sampleBytes := s.channels[id].GetSensor().GetSampleBytes()
		if !mcusource.ValidGenericPayloadSize(len(f.Payload), sampleBytes) {
			return false
		}
		var frame *sensorlinkpb.SensorData
		for sub := range s.subscribers {
			if !sub.ids[id] {
				continue
			}
			// One immutable copy shared by readers, and no allocation when nobody
			// consumes this channel. Never retain the transport's mutable buffer.
			if frame == nil {
				frame = &sensorlinkpb.SensorData{ChannelId: id, FrameSeq: f.Seq, TsUs: f.TsUs, Flags: f.Flags | sensorlink.FlagLastChunk, Payload: append([]byte(nil), f.Payload...)}
			}
			select {
			case sub.frames <- frame:
			default:
				// Do not let a stalled viewer block capture or accumulate stale samples.
				s.endSubscription(sub, status.Error(codes.ResourceExhausted, "sensor reader fell behind; reconnect"))
			}
		}
		return true
	}
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !active {
			return
		}
		active = false
		for _, id := range ids {
			delete(s.channels, id)
			for sub := range s.subscribers {
				if sub.ids[id] {
					s.endSubscription(sub, status.Error(codes.Unavailable, "sensor disconnected; refresh manifest"))
				}
			}
		}
	}
	return accepted, publish, release
}

func (s *GenericSensorFeed) GetSensorManifest(_ context.Context, _ *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := &sensorlinkpb.SensorManifest{}
	for _, d := range s.channels {
		result.Sensors = append(result.Sensors, proto.Clone(d).(*sensorlinkpb.SensorDescriptor))
	}
	sort.Slice(result.Sensors, func(i, j int) bool { return result.Sensors[i].ChannelId < result.Sensors[j].ChannelId })
	return result, nil
}

func (s *GenericSensorFeed) StreamSensors(req *agentpbv2.StreamSensorsRequest, stream grpc.ServerStreamingServer[sensorlinkpb.SensorData]) error {
	if len(req.GetChannelId()) == 0 || len(req.GetChannelId()) > 64 {
		return status.Error(codes.InvalidArgument, "select 1 to 64 sensor channels")
	}
	sub := &genericSubscription{ids: make(map[uint32]bool), frames: make(chan *sensorlinkpb.SensorData, 8), done: make(chan struct{})}
	s.mu.Lock()
	if len(s.subscribers) >= 64 {
		s.mu.Unlock()
		return status.Error(codes.ResourceExhausted, "sensor reader limit reached")
	}
	for _, id := range req.GetChannelId() {
		if s.channels[id] == nil {
			s.mu.Unlock()
			return status.Error(codes.NotFound, "sensor disconnected; refresh manifest")
		}
		sub.ids[id] = true
	}
	s.subscribers[sub] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, sub); s.mu.Unlock() }()
	// A reader can stall inside gRPC Send, not only while waiting for a sample.
	// Returning the RPC on overflow/disconnect cancels that Send in the transport.
	sendErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-sub.done:
				return
			case <-stream.Context().Done():
				return
			case f := <-sub.frames:
				if err := stream.Send(f); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	select {
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-sub.done:
		return sub.err
	case err := <-sendErr:
		return err
	}
}

// Caller holds mu. Publishing the error before closing done synchronizes readers.
func (s *GenericSensorFeed) endSubscription(sub *genericSubscription, err error) {
	sub.err = err
	close(sub.done)
	delete(s.subscribers, sub)
}
