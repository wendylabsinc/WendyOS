package unitree

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

func TestSlowVideoSubscriberRecoversAtIDRWithoutDamagingFastSubscriber(t *testing.T) {
	slow := &subscriber{video: make(chan VideoFrame, 2)}
	fast := &subscriber{video: make(chan VideoFrame, 2)}
	c := &Connection{subscribers: map[*subscriber]struct{}{slow: {}, fast: {}}}
	keyframe := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 1, 0x68, 0x80, 0, 0, 1, 0x65, 0x80}
	inter := []byte{0, 0, 1, 0x41, 0x80}
	for i, frame := range [][]byte{keyframe, inter, inter, inter, keyframe, inter} {
		c.dispatchVideo(VideoFrame{AnnexB: frame, RTPTimestamp: uint32(i)}, false)
		if got := <-fast.video; !bytes.Equal(got.AnnexB, frame) || got.RTPTimestamp != uint32(i) {
			t.Fatalf("fast subscriber lost picture %d: %+v", i, got)
		}
		if (i == 2 || i == 3) && len(slow.video) != 0 {
			t.Fatal("slow subscriber received a picture after losing its reference")
		}
	}
	if got := <-slow.video; got.RTPTimestamp != 4 || !bytes.Equal(got.AnnexB, keyframe) {
		t.Fatalf("recovery did not preserve complete keyframe and parameter sets: %+v", got)
	}
	if got := <-slow.video; got.RTPTimestamp != 5 {
		t.Fatalf("missing first dependent picture after recovery: %+v", got)
	}
}

func TestVideoSubscriberJoiningMidGOPWaitsForIDR(t *testing.T) {
	// A previous viewer already enabled the robot's track.
	c := &Connection{videoRefs: 1}
	c.connected.Store(true)
	_, video, cancel, err := c.Subscribe(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	inter := []byte{0, 0, 1, 0x41, 0x80}
	keyframe := []byte{0, 0, 1, 0x65, 0x80}
	c.dispatchVideo(VideoFrame{AnnexB: inter, RTPTimestamp: 1}, false)
	if len(video) != 0 {
		t.Fatal("new decoder received a picture without its reference")
	}
	c.dispatchVideo(VideoFrame{AnnexB: keyframe, RTPTimestamp: 2}, false)
	c.dispatchVideo(VideoFrame{AnnexB: inter, RTPTimestamp: 3}, false)
	for _, want := range []uint32{2, 3} {
		select {
		case got := <-video:
			if got.RTPTimestamp != want {
				t.Fatalf("timestamp = %d, want %d", got.RTPTimestamp, want)
			}
		default:
			t.Fatalf("missing picture %d after IDR", want)
		}
	}
}

func TestVideoRTPPacketLossWaitsForIDRWithoutQueuePressure(t *testing.T) {
	s := &subscriber{video: make(chan VideoFrame, videoBuffer)}
	c := &Connection{subscribers: map[*subscriber]struct{}{s: {}}}
	builder := samplebuilder.New(2, &codecs.H264Packet{}, 90000)
	var delivered []uint32
	sawLoss := false
	// Packet 3 (one complete reference picture) never arrives. Pion must
	// expire it before the next picture reports PrevDroppedPackets.
	for _, seq := range []uint16{1, 2, 4, 5, 6, 7, 8} {
		nal := byte(0x41)
		if seq == 1 || seq == 6 {
			nal = 0x65
		}
		builder.Push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 100, Marker: true}, Payload: []byte{nal, 0x80}})
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			lost := sample.PrevDroppedPackets > 0
			sawLoss = sawLoss || lost
			c.dispatchVideo(VideoFrame{AnnexB: sample.Data, RTPTimestamp: sample.PacketTimestamp}, lost)
			select {
			case got := <-s.video:
				delivered = append(delivered, got.RTPTimestamp)
			default:
			}
		}
	}
	if !sawLoss {
		t.Fatal("RTP fixture did not report the missing packet")
	}
	if want := []uint32{100, 200, 600, 700}; !reflect.DeepEqual(delivered, want) {
		t.Fatalf("delivered timestamps = %v, want %v (no dependent pictures after loss)", delivered, want)
	}
}

func TestH264KeyframeUsesAnnexBAccessUnits(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		key  bool
	}{
		{"empty", nil, false},
		{"inter", []byte{0, 0, 1, 0x41, 0x80}, false},
		{"three byte IDR", []byte{0, 0, 1, 0x65, 0x80}, true},
		{"four byte IDR", []byte{0, 0, 0, 1, 0x65, 0x80}, true},
		{"parameter sets only", []byte{0, 0, 1, 0x67, 0x42, 0, 0, 1, 0x68, 0x80}, false},
		{"header byte outside NAL", []byte{0, 0, 1, 0x41, 0x65, 0x80}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsH264Keyframe(tc.data); got != tc.key {
				t.Fatalf("keyframe = %v, want %v", got, tc.key)
			}
		})
	}
}
