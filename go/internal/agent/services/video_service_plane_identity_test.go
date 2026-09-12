package services

import (
	"sync/atomic"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// TestASecondPlaneDeliveryConsumesNoSampleIdentity crosses the two features
// that were, individually, both correct: the hub's per-source sample counter,
// and the hub's willingness to deliver one physical camera frame on more than
// one plane. The raw frame tap is the second plane in production — its
// publishRaw hands the uncompressed capture frame to analytic subscribers as
// its own videoFrame, through the same hub — and a per-broadcast counter made
// each camera frame burn two identifiers, so the encoded stream came out with
// identifiers [2 4 6] while the hub reported no drops at all.
//
// Two contracts break at once when that happens, and neither breaks loudly:
//
//   - hub_loopback_binding reads a jump in sample_id across a dense loopback
//     sequence as a producer-side drop, so every frame reads as a drop.
//   - the model-input ledger's sample_id is supposed to name the episode index
//     entry for the same frame, so a join over it returns nothing.
//
// The regression got through because the two features were tested in files
// that never met: the loopback and sensor tests never attach a second plane,
// and the raw tap's tests never look at sample identities. This test is the
// crossing. It asserts what a consumer actually relies on: with every frame
// delivered, the encoded identifiers are dense, and the drop counter agrees.
func TestASecondPlaneDeliveryConsumesNoSampleIdentity(t *testing.T) {
	hub, cancel := newSampleHub(new(atomic.Uint64))
	defer cancel()
	subID, encoded, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	// Marker bytes stand in for the two planes: the encoded frame a viewer,
	// the sensor subscribers and episode capture read, and the second delivery
	// of the same physical frame that the raw tap performs beside it.
	const encodedPlane, secondPlane = byte(0xE0), byte(0x5A)

	var ids []uint64
	for i := 0; i < 3; i++ {
		// One physical frame arriving from the producer.
		if !hub.produce(&videoFrame{
			data:      []byte{encodedPlane, byte(i)},
			codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
			auAligned: true,
		}) {
			t.Fatalf("frame %d: produce reported no subscribers", i)
		}
		// The same physical frame put onto a further plane, as its own
		// videoFrame. This is the shape of the raw tap's publishRaw.
		if !hub.broadcast(&videoFrame{data: []byte{secondPlane, byte(i)}}) {
			t.Fatalf("frame %d: the second plane delivery reported no subscribers", i)
		}
		// Drained every round so the subscriber never falls behind: a drop
		// here would be a legitimate reason for a gap, and this test is about
		// gaps that have no reason at all.
		for j := 0; j < 2; j++ {
			frame := <-encoded
			if frame.data[0] == encodedPlane {
				ids = append(ids, frame.sampleID)
			}
		}
	}

	// Dense, one per physical frame, whatever else the hub delivered.
	for i, id := range ids {
		if id != uint64(i+1) {
			t.Fatalf("encoded sample identifiers are %v, want [1 2 3]: a second plane delivery consumed an identifier, "+
				"so every frame now reads as a producer-side drop", ids)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("received %d encoded frame(s), want 3", len(ids))
	}
	// The corroborating half of the contract. A gap is only ever allowed to
	// mean a sample that existed and was lost, and the hub is the thing that
	// would have lost it.
	if drops := hub.drops(subID); drops != 0 {
		t.Fatalf("hub reported %d drop(s) for a subscriber that read every frame", drops)
	}
}
