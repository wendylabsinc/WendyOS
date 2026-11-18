package services

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// expectDemandPoke waits briefly for one demandChanged signal.
func expectDemandPoke(t *testing.T, changed <-chan struct{}, after string) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("demandChanged did not fire after %s", after)
	}
}

// drainDemand empties demandChanged so the next signal is attributable.
func drainDemand(changed <-chan struct{}) {
	for {
		select {
		case <-changed:
		default:
			return
		}
	}
}

func TestDeviceHub_PixelSubscriberGetsTapFramesOnly(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idEnc, chEnc, _ := h.subscribe()
	idRaw, chRaw, _ := h.subscribeKind(true)
	idPix, chPix, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("subscribePixels: %v", err)
	}
	defer h.unsubscribe(idEnc)
	defer h.unsubscribe(idRaw)
	defer h.unsubscribe(idPix)

	format := &agentpb.RawFormat{Width: 2, Height: 1, Fourcc: fourccYUYV, BytesPerLine: 4}
	h.pixelsOffered(format)
	h.rawOffered(format)

	if !h.publishRaw([]byte{9, 9, 9, 9}, 7, format) {
		t.Fatal("raw broadcast reported no subscribers")
	}
	if len(chRaw) != 1 || len(chPix) != 1 || len(chEnc) != 0 {
		t.Fatalf("after a tap frame: raw=%d pixels=%d encoded=%d queued, want 1/1/0", len(chRaw), len(chPix), len(chEnc))
	}

	if !h.produce(&videoFrame{data: []byte{0, 0, 0, 1, 0x65}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatal("encoded produce reported no subscribers")
	}
	if len(chEnc) != 1 || len(chRaw) != 1 || len(chPix) != 1 {
		t.Fatalf("after an encoded frame: encoded=%d raw=%d pixels=%d queued, want 1/1/1", len(chEnc), len(chRaw), len(chPix))
	}

	f := <-chPix
	if f.codec != agentpb.VideoCodec_VIDEO_CODEC_RAW || f.rawFmt != format || f.tsNs != 7 {
		t.Errorf("pixel subscriber got %v (rawFmt=%v ts=%d)", f.codec, f.rawFmt, f.tsNs)
	}
	if f = <-chEnc; f.codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
		t.Errorf("encoded subscriber got %v", f.codec)
	}
}

func TestDeviceHub_WantEncodedTracksOnlyEncodedSubscribers(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idPix, _, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("subscribePixels: %v", err)
	}
	defer h.unsubscribe(idPix)
	if h.wantEncoded() {
		t.Error("a pixel-only hub must not want encoded video")
	}
	if !h.wantRaw() {
		t.Error("a pixel subscriber must make the tap wanted")
	}

	idEnc, _, _ := h.subscribe()
	if !h.wantEncoded() {
		t.Error("an encoded subscriber must make encoded video wanted")
	}
	h.unsubscribe(idEnc)
	if h.wantEncoded() {
		t.Error("encoded video still wanted after the only encoded subscriber left")
	}
	if !h.wantRaw() {
		t.Error("the pixel subscriber stopped counting when the encoded one left")
	}
}

func TestDeviceHub_DemandChangedFiresOnJoinAndLeave(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	// Keeps the hub alive: the last unsubscribe cancels it.
	idHold, _, _ := h.subscribe()
	defer h.unsubscribe(idHold)

	changed := h.demandChanged()
	if cap(changed) != 1 {
		t.Fatalf("demandChanged capacity %d, want 1", cap(changed))
	}
	drainDemand(changed)

	idPix, _, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("subscribePixels: %v", err)
	}
	expectDemandPoke(t, changed, "a subscribe")
	h.unsubscribe(idPix)
	expectDemandPoke(t, changed, "an unsubscribe")

	// Nobody reads between these two joins. A poke that blocked on the full
	// channel would hang the second subscribe while it holds the hub mutex.
	done := make(chan struct{})
	var ids []int
	go func() {
		defer close(done)
		for range 2 {
			id, _, err := h.subscribePixels()
			if err != nil {
				t.Errorf("subscribePixels: %v", err)
				return
			}
			ids = append(ids, id)
		}
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		drainDemand(changed) // free the stuck join so the deferred unsubscribe can lock
		t.Fatal("two joins with no receive between them blocked")
	}
	for _, id := range ids {
		h.unsubscribe(id)
	}
	expectDemandPoke(t, changed, "unread joins and leaves")
}

func TestDeviceHub_NoTapTurnsAwayPixelSubscribersToo(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idEnc, chEnc, _ := h.subscribe()
	idPix, chPix, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("subscribePixels: %v", err)
	}
	defer h.unsubscribe(idEnc)
	defer h.unsubscribe(idPix)
	changed := h.demandChanged()
	drainDemand(changed)

	const reason = "camera streams H.264 natively; raw frames are not offered"
	h.rawNotOffered(fixedReason(reason))

	if _, ok := <-chPix; ok {
		t.Fatal("pixel subscriber channel should be closed")
	}
	refusal := h.subscriberErr(idPix)
	if !errors.Is(refusal, errPixelsUnavailable) {
		t.Fatalf("pixel subscriber error %v is not errPixelsUnavailable", refusal)
	}
	if !strings.Contains(refusal.Error(), reason) {
		t.Errorf("pixel refusal must carry the producer's reason: %v", refusal)
	}
	if h.subscriberErr(idEnc) != nil {
		t.Error("encoded subscriber must not inherit the pixel refusal")
	}
	if h.wantRaw() {
		t.Error("a turned-away pixel subscriber must not count as wanting the tap")
	}
	expectDemandPoke(t, changed, "the producer closed a pixel subscriber")

	// The viewer is still attached and still served.
	if !h.broadcast(&videoFrame{data: []byte{1}, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatal("broadcast stopped the producer while an encoded subscriber remains")
	}
	select {
	case _, ok := <-chEnc:
		if !ok {
			t.Fatal("encoded subscriber channel was closed by the pixel refusal")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("encoded subscriber stopped receiving after the pixel refusal")
	}

	_, _, err = h.subscribePixels()
	if !errors.Is(err, errPixelsUnavailable) || err.Error() != refusal.Error() {
		t.Errorf("a later pixel join got %v, want the same refusal %v", err, refusal)
	}
}

func TestDeviceHub_RawRefusedButPixelsOfferedOnMJPEG(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()

	idRaw, chRaw, _ := h.subscribeKind(true)
	idPix, chPix, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("subscribePixels: %v", err)
	}
	defer h.unsubscribe(idRaw)
	defer h.unsubscribe(idPix)

	decoded := &agentpb.RawFormat{Width: 1280, Height: 720, Fourcc: fourccYUYV, BytesPerLine: 2560}
	h.pixelsOffered(decoded)
	h.rawNotOffered(fixedReason("camera is captured as MJPEG at 1280x720; raw frames are not offered"))

	// A gRPC raw subscriber was promised the bytes the camera produced; decoded
	// MJPEG is not that, so it is still turned away.
	if _, ok := <-chRaw; ok {
		t.Fatal("raw subscriber channel should be closed")
	}
	rawErr := h.subscriberErr(idRaw)
	if st, _ := status.FromError(rawErr); st.Code() != codes.FailedPrecondition || !streamreason.Has(rawErr, streamreason.RawUnavailable) {
		t.Errorf("raw subscriber error %v, want FailedPrecondition with %s", rawErr, streamreason.RawUnavailable)
	}
	if !strings.Contains(rawErr.Error(), "MJPEG") {
		t.Errorf("raw refusal must carry the producer's reason: %v", rawErr)
	}
	if h.subscriberErr(idPix) != nil {
		t.Errorf("pixel subscriber was refused on a camera that offers decoded frames: %v", h.subscriberErr(idPix))
	}

	if !h.publishRaw(make([]byte, 4), 11, decoded) {
		t.Fatal("tap frame reported no subscribers")
	}
	select {
	case f, ok := <-chPix:
		if !ok {
			t.Fatal("pixel subscriber channel was closed")
		}
		if f.rawFmt != decoded {
			t.Errorf("pixel subscriber got rawFmt %v, want %v", f.rawFmt, decoded)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pixel subscriber received nothing")
	}

	if h.rawRefusal() == nil {
		t.Error("a later raw join must be refused")
	}
	idLate, chLate, err := h.subscribePixels()
	if err != nil {
		t.Fatalf("a later pixel join was refused: %v", err)
	}
	defer h.unsubscribe(idLate)
	h.publishRaw(make([]byte, 4), 12, decoded)
	select {
	case <-chLate:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("late pixel subscriber received nothing")
	}
}

func TestDeviceHub_EncodedStreamRestartedClearsWebMState(t *testing.T) {
	h, cancel := newTestHub(t)
	defer cancel()
	_, ch, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	// Each initialization is an EBML header and an unknown-size Segment, ended
	// by the first Cluster; B differs from A by a Void inside its Segment.
	initA := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x18, 0x53, 0x80, 0x67, 0xff}
	initB := append(append([]byte(nil), initA...), 0xec, 0x81, 0x00)
	cluster := []byte{0x1f, 0x43, 0xb6, 0x75, 0x80}
	vp8 := func(data ...[]byte) *videoFrame {
		return &videoFrame{codec: agentpb.VideoCodec_VIDEO_CODEC_VP8, data: bytes.Join(data, nil)}
	}

	h.broadcast(vp8(initA, cluster))
	if f := <-ch; !bytes.Equal(f.decoderInit, initA) {
		t.Fatalf("first stream's initialization %x, want %x", f.decoderInit, initA)
	}

	h.encodedStreamRestarted()
	h.broadcast(vp8(initB, cluster))
	first := <-ch
	if !bytes.Equal(first.decoderInit, initB) {
		t.Fatalf("restarted stream's first frame carries initialization %x, want %x", first.decoderInit, initB)
	}
	if first.webmJoin != webmJoinAsIs {
		t.Errorf("restarted stream's first frame is not a stream start: join %+v", first.webmJoin)
	}
	h.broadcast(vp8([]byte("mid-stream encoded payload")))
	if next := <-ch; !bytes.Equal(next.decoderInit, initB) {
		t.Fatalf("next frame carries initialization %x, want the restarted stream's %x", next.decoderInit, initB)
	}
}

func TestGetOrCreateHub_PixelJoinRefusedUpFrontOnceProducerDeclined(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	installFakeProducers(svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, idPix, chPix, err := svc.joinHubPixels(ctx, "/dev/video0", &agentpb.StreamVideoRequest{})
	if err != nil {
		t.Fatalf("joinHubPixels on an idle camera: %v", err)
	}
	defer h.unsubscribe(idPix)
	if h.wantEncoded() || !h.wantRaw() {
		t.Errorf("hub started by a pixel join: wantEncoded=%v wantRaw=%v, want false/true", h.wantEncoded(), h.wantRaw())
	}

	// A pixel join does not insist on parameters: it joins whatever runs.
	h2, idPix2, _, err := svc.joinHubPixels(ctx, "/dev/video0", &agentpb.StreamVideoRequest{Width: 1280, Height: 720, Framerate: 30})
	if err != nil {
		t.Fatalf("pixel join with other parameters was refused: %v", err)
	}
	if h2 != h {
		t.Fatal("pixel join with other parameters created a second hub")
	}
	h2.unsubscribe(idPix2)

	_, idEnc, _, err := svc.joinHub(ctx, "/dev/video0", &agentpb.StreamVideoRequest{})
	if err != nil {
		t.Fatalf("joinHub: %v", err)
	}
	defer h.unsubscribe(idEnc)

	h.rawNotOffered(fixedReason("CSI cameras are captured through the ISP pipeline; raw frames are not offered"))
	if _, ok := <-chPix; ok {
		t.Fatal("waiting pixel subscriber was not turned away")
	}

	_, _, _, err = svc.joinHubPixels(ctx, "/dev/video0", &agentpb.StreamVideoRequest{})
	if !errors.Is(err, errPixelsUnavailable) {
		t.Fatalf("pixel join on a hub that declined pixels got %v, want errPixelsUnavailable", err)
	}
	if !strings.Contains(err.Error(), "ISP pipeline") {
		t.Errorf("refusal must carry the producer's reason: %v", err)
	}
}
