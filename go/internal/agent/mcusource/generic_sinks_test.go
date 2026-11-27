package mcusource

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// pickySink takes only some channels and records what it is handed.
type pickySink struct {
	accept   map[uint32]bool
	refuse   map[uint32]bool // frames of these channels are not usable
	mounted  []uint32
	frames   []uint32
	released int
}

func (s *pickySink) Mount(_ SensorPairing, ds []*sensorpb.SensorDescriptor) ([]uint32, func(*sensorlink.SensorFrame) bool, func()) {
	var ids []uint32
	for _, d := range ds {
		s.mounted = append(s.mounted, d.ChannelId)
		if s.accept[d.ChannelId] {
			ids = append(ids, d.ChannelId)
		}
	}
	publish := func(f *sensorlink.SensorFrame) bool {
		s.frames = append(s.frames, f.ChannelID)
		return !s.refuse[f.ChannelID]
	}
	return ids, publish, func() { s.released++ }
}

func genericDescriptors(ids ...uint32) []*sensorpb.SensorDescriptor {
	out := make([]*sensorpb.SensorDescriptor, 0, len(ids))
	for _, id := range ids {
		out = append(out, &sensorpb.SensorDescriptor{ChannelId: id, Format: &sensorpb.SensorDescriptor_Sensor{
			Sensor: &sensorpb.SensorFormat{Schema: "example", SampleBytes: 1}}})
	}
	return out
}

func TestGenericSinksGivesEachSinkTheChannelsItAccepted(t *testing.T) {
	apps := &pickySink{accept: map[uint32]bool{1: true, 2: true}}
	ros := &pickySink{accept: map[uint32]bool{2: true, 3: true}}
	accepted, publish, release := GenericSinks(apps, ros).Mount(SensorPairing{SourceAssetID: 7}, genericDescriptors(1, 2, 3, 4))

	// Every sink is offered every channel, and a channel is subscribed to
	// when at least one sink takes it. Nobody took channel 4.
	if !reflect.DeepEqual(apps.mounted, []uint32{1, 2, 3, 4}) || !reflect.DeepEqual(ros.mounted, []uint32{1, 2, 3, 4}) {
		t.Fatalf("offered apps %v and ros %v, want all four channels each", apps.mounted, ros.mounted)
	}
	if !reflect.DeepEqual(accepted, []uint32{1, 2, 3}) {
		t.Fatalf("accepted %v, want [1 2 3]", accepted)
	}

	var delivered []bool
	for _, id := range []uint32{1, 2, 3, 4} {
		delivered = append(delivered, publish(&sensorlink.SensorFrame{ChannelID: id, Payload: []byte{1}}))
	}
	// A sink only sees frames of the channels it accepted.
	if !reflect.DeepEqual(apps.frames, []uint32{1, 2}) || !reflect.DeepEqual(ros.frames, []uint32{2, 3}) {
		t.Fatalf("apps got frames of %v and ros of %v", apps.frames, ros.frames)
	}
	if !reflect.DeepEqual(delivered, []bool{true, true, true, false}) {
		t.Fatalf("delivered = %v", delivered)
	}

	release()
	release()
	if apps.released != 1 || ros.released != 1 {
		t.Fatalf("released apps %d times and ros %d times, want once each", apps.released, ros.released)
	}
}

func TestGenericSinksDeliversWhenAnySinkCanUseTheFrame(t *testing.T) {
	broken := &pickySink{accept: map[uint32]bool{5: true}, refuse: map[uint32]bool{5: true}}
	working := &pickySink{accept: map[uint32]bool{5: true}}
	_, publish, release := GenericSinks(broken, working).Mount(SensorPairing{SourceAssetID: 7}, genericDescriptors(5))
	defer release()
	if !publish(&sensorlink.SensorFrame{ChannelID: 5, Payload: []byte{1}}) {
		t.Fatal("a frame one sink could not use was reported as undelivered although the other took it")
	}
	if len(broken.frames) != 1 || len(working.frames) != 1 {
		t.Fatalf("broken saw %d frames and working %d, want one each", len(broken.frames), len(working.frames))
	}
	onlyBroken := &pickySink{accept: map[uint32]bool{5: true}, refuse: map[uint32]bool{5: true}}
	_, publish, release2 := GenericSinks(onlyBroken, &pickySink{}).Mount(SensorPairing{SourceAssetID: 7}, genericDescriptors(5))
	defer release2()
	if publish(&sensorlink.SensorFrame{ChannelID: 5, Payload: []byte{1}}) {
		t.Fatal("a frame no sink could use was reported as delivered")
	}
}

func TestGenericSinksWithoutAChoiceIsTheSinkItself(t *testing.T) {
	// The supervisor subscribes to generic channels only when it has a sink,
	// so no sink at all has to stay a nil interface.
	if sink := GenericSinks(); sink != nil {
		t.Fatalf("no sinks gave %T, want nil", sink)
	}
	if sink := GenericSinks(nil, nil); sink != nil {
		t.Fatalf("nil sinks gave %T, want nil", sink)
	}
	only := &pickySink{}
	if sink := GenericSinks(nil, only); sink != GenericSink(only) {
		t.Fatalf("one sink gave %T, want the sink itself", sink)
	}
}
