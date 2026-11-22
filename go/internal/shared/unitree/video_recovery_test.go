package unitree

import (
	"bytes"
	"testing"
)

func TestSlowVideoSubscriberRecoversAtIDRWithoutDamagingFastSubscriber(t *testing.T) {
	slow := &subscriber{video: make(chan VideoFrame, 2)}
	fast := &subscriber{video: make(chan VideoFrame, 2)}
	c := &Connection{subscribers: map[*subscriber]struct{}{slow: {}, fast: {}}}
	keyframe := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 1, 0x68, 0x80, 0, 0, 1, 0x65, 0x80}
	inter := []byte{0, 0, 1, 0x41, 0x80}
	for i, frame := range [][]byte{keyframe, inter, inter, inter, keyframe, inter} {
		c.dispatchVideo(VideoFrame{AnnexB: frame, RTPTimestamp: uint32(i)})
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
