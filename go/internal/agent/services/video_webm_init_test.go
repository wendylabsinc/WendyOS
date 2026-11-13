package services

import (
	"bytes"
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestWebMInitializationAcrossChunkBoundaries(t *testing.T) {
	// EBML header, unknown-size Segment, metadata containing a false Cluster ID,
	// then the real Cluster. IDs inside metadata must not cut the header short.
	header := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xff, 0xec, 0x84, 0x1f, 0x43, 0xb6, 0x75}
	stream := append(append([]byte(nil), header...), 0x1f, 0x43, 0xb6, 0x75, 0x80)
	for chunk := 1; chunk <= len(stream); chunk++ {
		var init webmInitialization
		for off := 0; off < len(stream); off += chunk {
			init.feed(stream[off:min(off+chunk, len(stream))])
		}
		if !bytes.Equal(init.header, header) {
			t.Fatalf("chunk %d: header %x, want %x", chunk, init.header, header)
		}
		init.feed([]byte("later cluster payload"))
		if !bytes.Equal(init.header, header) || init.pending != nil {
			t.Fatal("initialization grew after first Cluster")
		}
	}
}

func TestWebMInitializationBoundedAndRejectsInvalidPrefix(t *testing.T) {
	for _, input := range [][]byte{bytes.Repeat([]byte{0}, maxWebMInitialization+100), {0x1a, 0x45, 0xdf, 0xa3, 0xff}, {0x81, 0x80}} {
		var init webmInitialization
		init.feed(input)
		if !init.failed || len(init.pending) != 0 || len(init.header) != 0 {
			t.Fatal("invalid initialization retained")
		}
	}
}

func TestWebMLateSubscriberReceivesInitializationWithoutChangingSample(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := &deviceHub{ctx: ctx, subs: make(map[int]*hubSubscriber)}
	_, _, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	header := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xff}
	hub.broadcast(&videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: append(append([]byte(nil), header...), 0x1f, 0x43, 0xb6, 0x75, 0x80)})
	id, ch, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("mid-stream encoded payload")
	hub.broadcast(&videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: payload, sampleID: 91})
	sub := cameraSensorSubscription{hub: hub, subID: id, frames: ch}
	sample, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sample.SampleID != 91 || !bytes.Equal(sample.Payload, payload) || !bytes.Equal(sample.DecoderInit, header) {
		t.Fatalf("sample identity or initialization lost: %+v", sample)
	}
}

// webmTestCluster records where a test Cluster starts in the stream, where the
// VP8 frame tag of its first block sits, and whether that block is a key frame.
type webmTestCluster struct {
	start, tag int
	key        bool
}

// webmTestStream builds a stream shaped like webmmux streamable=true output:
// unknown-size Segment and Clusters, plus a known-size Cluster, a BlockGroup
// and a Void so every form is walked. Block payloads are full of bytes that
// would parse as Cluster and EBML identifiers if they were not skipped.
func webmTestStream() (stream, init []byte, clusters []webmTestCluster) {
	el := func(id []byte, body ...[]byte) []byte {
		payload := bytes.Join(body, nil)
		out := append([]byte(nil), id...)
		if len(payload) < 0x7f {
			out = append(out, 0x80|byte(len(payload)))
		} else {
			out = append(out, 0x40|byte(len(payload)>>8), byte(len(payload)))
		}
		return append(out, payload...)
	}
	clusterID := []byte{0x1f, 0x43, 0xb6, 0x75}
	unknown := []byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	// block returns a SimpleBlock (0xa3) or Block (0xa1) and the offset of its
	// VP8 frame tag: track 1, timecode, flags, then the tag.
	block := func(id byte, key bool, repeats int) ([]byte, int) {
		flags, tag := byte(0), byte(0x01)
		if key {
			tag = 0x00
			if id == 0xa3 {
				flags = 0x80
			}
		}
		filler := bytes.Repeat([]byte{0x1f, 0x43, 0xb6, 0x75, 0x1a, 0x45, 0xdf, 0xa3, 0x00}, repeats)
		e := el([]byte{id}, []byte{0x81, 0x00, 0x00, flags, tag}, filler)
		return e, len(e) - len(filler) - 1
	}
	timecode := func(v byte) []byte { return el([]byte{0xe7}, []byte{v}) }

	stream = append(stream, el([]byte{0x1a, 0x45, 0xdf, 0xa3}, el([]byte{0x42, 0x82}, []byte("webm")))...)
	stream = append(append(stream, 0x18, 0x53, 0x80, 0x67), unknown...)
	stream = append(stream, el([]byte{0x15, 0x49, 0xa9, 0x66}, el([]byte{0x2a, 0xd7, 0xb1}, []byte{0x0f, 0x42, 0x40}))...)
	stream = append(stream, el([]byte{0x16, 0x54, 0xae, 0x6b}, el([]byte{0xae}, el([]byte{0xd7}, []byte{1}), el([]byte{0x86}, []byte("V_VP8"))))...)
	init = append([]byte(nil), stream...)

	// Unknown-size Cluster opening on a key frame, then a long delta frame.
	start := len(stream)
	key, tag := block(0xa3, true, 2)
	delta, _ := block(0xa3, false, 30)
	stream = append(append(append(append(append(stream, clusterID...), unknown...), timecode(0)...), key...), delta...)
	clusters = append(clusters, webmTestCluster{start, start + len(clusterID) + len(unknown) + 3 + tag, true})

	// Known-size Cluster opening on a delta frame; its later key frame is not
	// a place a decoder can start a Cluster from.
	start = len(stream)
	delta, tag = block(0xa3, false, 2)
	key, _ = block(0xa3, true, 2)
	known := el(clusterID, timecode(1), delta, key)
	stream = append(stream, known...)
	clusters = append(clusters, webmTestCluster{start, start + len(known) - len(delta) - len(key) + tag, false})

	// Unknown-size Cluster whose first frame is a Block inside a BlockGroup,
	// after a Void.
	start = len(stream)
	void := el([]byte{0xec}, []byte{0, 0, 0})
	key, tag = block(0xa1, true, 2)
	group := el([]byte{0xa0}, key, el([]byte{0x9b}, []byte{33}))
	delta, _ = block(0xa3, false, 2)
	stream = append(append(append(append(append(append(stream, clusterID...), unknown...), timecode(2)...), void...), group...), delta...)
	clusters = append(clusters, webmTestCluster{start, start + len(clusterID) + len(unknown) + 3 + len(void) + len(group) - len(key) - 3 + tag, true})

	start = len(stream)
	key, tag = block(0xa3, true, 2)
	stream = append(append(append(append(stream, clusterID...), unknown...), timecode(3)...), key...)
	clusters = append(clusters, webmTestCluster{start, start + len(clusterID) + len(unknown) + 3 + tag, true})
	return stream, init, clusters
}

// lateWebMViewer is what a viewer whose first read starts at joinOffset should
// receive: the whole stream if that is its start, otherwise the initialization
// and the first Cluster opening on a key frame whose frame tag it was there to
// see.
func lateWebMViewer(stream, init []byte, clusters []webmTestCluster, joinOffset int) []byte {
	if joinOffset == 0 {
		return stream
	}
	for _, c := range clusters {
		if c.key && c.tag >= joinOffset {
			return append(append([]byte(nil), init...), stream[c.start:]...)
		}
	}
	return nil
}

func TestWebMTestStreamLayout(t *testing.T) {
	stream, init, clusters := webmTestStream()
	for _, c := range clusters {
		if !bytes.Equal(stream[c.start:c.start+4], []byte{0x1f, 0x43, 0xb6, 0x75}) {
			t.Fatalf("cluster at %d does not start with a Cluster ID", c.start)
		}
		if !bytes.Equal(stream[c.tag-4:c.tag-1], []byte{0x81, 0x00, 0x00}) {
			t.Fatalf("frame tag at %d does not follow a block header", c.tag)
		}
		if got := stream[c.tag]&0x01 == 0; got != c.key {
			t.Fatalf("frame tag at %d: key %v, want %v", c.tag, got, c.key)
		}
	}
	if !bytes.Equal(stream[:clusters[0].start], init) {
		t.Fatal("initialization is not the bytes before the first Cluster")
	}
}

func TestWebMJoinsFindKeyframeClustersAcrossReadBoundaries(t *testing.T) {
	stream, init, clusters := webmTestStream()
	for size := 1; size <= len(stream); size++ {
		var webmInit webmInitialization
		var joins webmJoins
		var reads [][]byte
		var points []*webmJoinPoint
		for off := 0; off < len(stream); off += size {
			read := stream[off:min(off+size, len(stream))]
			reads = append(reads, read)
			points = append(points, joins.feed(read, webmInit.feed(read)))
		}
		if !bytes.Equal(webmInit.header, init) || joins.failed {
			t.Fatalf("read size %d: initialization %x failed %v", size, webmInit.header, joins.failed)
		}
		for from := range reads {
			var got []byte
			joined := false
			for k := from; k < len(reads); k++ {
				if joined {
					got = append(got, reads[k]...)
				} else if points[k] != nil {
					joined = true
					got = points[k].join(reads[k])
				}
			}
			if want := lateWebMViewer(stream, init, clusters, from*size); !bytes.Equal(got, want) {
				t.Fatalf("read size %d, joined at read %d:\n got %x\nwant %x", size, from, got, want)
			}
		}
	}
}

func TestWebMJoinsPassThroughAStreamItCannotFollow(t *testing.T) {
	var joins webmJoins
	if joins.feed([]byte{0x00, 0x01}, nil) != webmJoinAsIs || !joins.failed {
		t.Fatal("unparseable stream was not passed through")
	}
	if joins.feed([]byte("later bytes"), nil) != webmJoinAsIs {
		t.Fatal("later frames of an unparseable stream were held back")
	}
}

// subscribeDeep subscribes with a channel deep enough for a whole test stream,
// so a viewer drained only afterwards drops nothing.
func subscribeDeep(t *testing.T, h *deviceHub) (int, chan *videoFrame) {
	t.Helper()
	id, _, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan *videoFrame, 1024)
	h.mu.Lock()
	h.subs[id].ch = ch
	h.mu.Unlock()
	return id, ch
}

// pumpClosed runs the view RPC's send loop over everything queued for one
// subscriber and returns the messages it sent.
func pumpClosed(h *deviceHub, id int, ch chan *videoFrame) [][]byte {
	close(ch)
	stream := newFakeVideoStream(context.Background())
	_ = (&VideoService{}).pumpFrames(stream, h, id, ch)
	return stream.frames
}

func TestVP8LateViewerStartsWithInitializationAtKeyframeCluster(t *testing.T) {
	stream, init, clusters := webmTestStream()
	h, cancel := newTestHub(t)
	defer cancel()
	firstID, first := subscribeDeep(t, h)

	// The late viewer joins inside the first Cluster's delta frame. The next
	// Cluster opens on a delta frame, so it must start at the BlockGroup one.
	const size = 7
	joinAt := clusters[0].tag/size + 1
	if joinAt*size > clusters[2].tag || joinAt*size <= clusters[0].tag {
		t.Fatalf("join read %d is not between the first and third Clusters", joinAt)
	}
	var lateID int
	var late chan *videoFrame
	for k, off := 0, 0; off < len(stream); k, off = k+1, off+size {
		if k == joinAt {
			lateID, late = subscribeDeep(t, h)
		}
		h.broadcast(&videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: stream[off:min(off+size, len(stream))]})
	}

	if got := bytes.Join(pumpClosed(h, firstID, first), nil); !bytes.Equal(got, stream) {
		t.Fatalf("first viewer's stream changed:\n got %x\nwant %x", got, stream)
	}
	sent := pumpClosed(h, lateID, late)
	if len(sent) == 0 || !bytes.HasPrefix(sent[0], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		t.Fatalf("late viewer's first message does not open with the EBML header: %x", sent)
	}
	want := append(append([]byte(nil), init...), stream[clusters[2].start:]...)
	if got := bytes.Join(sent, nil); !bytes.Equal(got, want) {
		t.Fatalf("late viewer did not start at the key frame Cluster:\n got %x\nwant %x", got, want)
	}
}

func TestH264LateViewerReceivesFramesUnchanged(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	firstID, first := subscribeDeep(t, h)
	frames := [][]byte{
		{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88},
		{0, 0, 0, 1, 0x41, 0x9a},
		{0, 0, 0, 1, 0x41, 0x9b},
		{0, 0, 0, 1, 0x41, 0x9c},
	}
	var lateID int
	var late chan *videoFrame
	for k, data := range frames {
		if k == 2 {
			lateID, late = subscribeDeep(t, h)
		}
		frame := &videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_H264, data: data}
		h.broadcast(frame)
		if frame.webmJoin != nil || frame.decoderInit != nil {
			t.Fatal("H.264 frame annotated with WebM state")
		}
	}
	for name, got := range map[string][][]byte{"first": pumpClosed(h, firstID, first), "late": pumpClosed(h, lateID, late)} {
		want := frames
		if name == "late" {
			want = frames[2:]
		}
		if len(got) != len(want) {
			t.Fatalf("%s viewer: sent %d frames, want %d", name, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("%s viewer frame %d: %x, want %x", name, i, got[i], want[i])
			}
		}
	}
}
