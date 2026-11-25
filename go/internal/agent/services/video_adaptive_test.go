package services

import (
	"context"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestRawFramePacerSlowsUnderBackpressureAndRecovers(t *testing.T) {
	var p rawFramePacer
	now := time.Unix(100, 0)
	if !p.allow(now) || p.fps != 5 {
		t.Fatal("first frame must start at 5 fps")
	}
	p.sent(now, now.Add(300*time.Millisecond))
	if p.fps >= 2 || p.allow(now.Add(400*time.Millisecond)) {
		t.Fatalf("blocked write did not reduce delivery: %+v", p)
	}
	slow := p.fps
	for i := 1; i < 10; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if p.allow(at) {
			p.sent(at, at.Add(time.Millisecond))
		}
	}
	if p.fps != slow {
		t.Fatal("increased before recovery window")
	}
	for i := 11; i < 200; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if p.allow(at) {
			p.sent(at, at.Add(time.Millisecond))
		}
	}
	if p.fps != 30 {
		t.Fatalf("healthy path did not recover to upper bound: %v", p.fps)
	}
	p.sent(now.Add(200*time.Second), now.Add(205*time.Second))
	if p.fps != 1 {
		t.Fatalf("long stall must reach 1 fps, got %v", p.fps)
	}
}

func TestAdaptiveRawDeliveryKeepsNewestFrameWithoutChangingOtherSubscribers(t *testing.T) {
	for _, tc := range []struct {
		name          string
		adaptive, raw bool
		want          int
	}{
		{"adaptive raw", true, true, 1},
		{"full-rate raw", false, true, 4},
		{"encoded", true, false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cancel := newTestHub(t)
			defer cancel()
			id, ch, err := h.subscribeKind(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			otherID, other, err := h.subscribeKind(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			defer h.unsubscribe(otherID)
			codec := pb.VideoCodec_VIDEO_CODEC_H264
			if tc.raw {
				codec = pb.VideoCodec_VIDEO_CODEC_RAW
			}
			for i := 0; i < 4; i++ {
				h.broadcast(&videoFrame{data: []byte{byte(i)}, codec: codec})
			}
			close(ch)
			stream := newFakeVideoStream(context.Background())
			_ = (&VideoService{}).pumpFrames(stream, h, id, ch, tc.adaptive)
			if len(stream.frames) != tc.want {
				t.Fatalf("sent %d frames, want %d", len(stream.frames), tc.want)
			}
			if tc.adaptive && tc.raw && stream.frames[0][0] != 3 {
				t.Fatal("sent stale backlog")
			}
			for i := 0; i < 4; i++ {
				select {
				case f := <-other:
					if f.data[0] != byte(i) {
						t.Fatal("other subscriber lost frame")
					}
				default:
					t.Fatal("adaptive viewer altered other subscriber")
				}
			}
		})
	}
}
