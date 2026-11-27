package mcusource

import (
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

// MaxGenericFrameBytes limits a frame containing one or more generic samples.
const MaxGenericFrameBytes = 64 << 10

// ValidGenericDescriptor bounds opaque fixed-size samples before subscription.
// No schema allowlist: interpretation belongs to the consuming application.
// A zero rate is allowed for event-driven sources; zero/unspecified sample size
// needs a shared protocol contract before it can be forwarded safely.
func ValidGenericDescriptor(d *sensorlinkpb.SensorDescriptor) bool {
	f := d.GetSensor()
	return f != nil && len(f.GetSchema()) > 0 && len(f.GetSchema()) <= 1024 &&
		len(d.GetName()) <= 256 && proto.Size(d) <= 2048 && f.GetSampleBytes() > 0 && f.GetSampleBytes() <= MaxGenericFrameBytes
}

// ValidGenericPayloadSize reports whether a bounded frame holds whole samples.
func ValidGenericPayloadSize(payloadBytes int, sampleBytes uint32) bool {
	return sampleBytes > 0 && payloadBytes > 0 && payloadBytes <= MaxGenericFrameBytes && payloadBytes%int(sampleBytes) == 0
}

// GenericSinks returns one sink that offers a source's generic channels to
// each of sinks. A channel is subscribed to when at least one of them accepts
// it, and each sink is handed only the frames of the channels it accepted, so
// one sink refusing a channel does not take it away from another. Nil sinks
// are skipped. With none left the result is nil, which is how the supervisor
// is told that generic channels have nowhere to go.
func GenericSinks(sinks ...GenericSink) GenericSink {
	var present genericSinks
	for _, sink := range sinks {
		if sink != nil {
			present = append(present, sink)
		}
	}
	switch len(present) {
	case 0:
		return nil
	case 1:
		return present[0]
	}
	return present
}

type genericSinks []GenericSink

func (g genericSinks) Mount(p SensorPairing, descriptors []*sensorlinkpb.SensorDescriptor) ([]uint32, func(*sensorlink.SensorFrame) bool, func()) {
	type mount struct {
		channels map[uint32]bool
		publish  func(*sensorlink.SensorFrame) bool
		release  func()
	}
	mounts := make([]mount, 0, len(g))
	taken := make(map[uint32]bool)
	for _, sink := range g {
		ids, publish, release := sink.Mount(p, descriptors)
		channels := make(map[uint32]bool, len(ids))
		for _, id := range ids {
			channels[id] = true
			taken[id] = true
		}
		mounts = append(mounts, mount{channels: channels, publish: publish, release: release})
	}
	// In the order the channels were offered, each once.
	accepted := make([]uint32, 0, len(taken))
	for _, d := range descriptors {
		if id := d.GetChannelId(); taken[id] {
			accepted = append(accepted, id)
			delete(taken, id)
		}
	}
	publish := func(f *sensorlink.SensorFrame) bool {
		if f == nil {
			return false
		}
		delivered := false
		for _, m := range mounts {
			if m.channels[f.ChannelID] && m.publish != nil && m.publish(f) {
				delivered = true
			}
		}
		return delivered
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, m := range mounts {
				if m.release != nil {
					m.release()
				}
			}
		})
	}
	return accepted, publish, release
}
