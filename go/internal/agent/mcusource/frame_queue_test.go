package mcusource

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

func TestCameraFrameQueuePreservesFIFOAndIndependentLatestSlots(t *testing.T) {
	q := newCameraFrameQueue([]uint32{1, 2})
	// A saturated audio/H.264 FIFO must not prevent either camera from
	// retaining its latest frame or let camera churn evict FIFO frames.
	for seq := range uint32(sensorFIFOFrames) {
		if q.offer(&sensorlink.SensorFrame{ChannelID: 3 + seq%2, Seq: seq}) {
			t.Fatal("early FIFO drop")
		}
	}
	for seq := range uint32(100) {
		for _, ch := range []uint32{1, 2} {
			if dropped := q.offer(&sensorlink.SensorFrame{ChannelID: ch, Seq: seq}); dropped != (seq > 0) {
				t.Fatalf("channel %d seq %d: dropped=%v", ch, seq, dropped)
			}
		}
	}
	if !q.offer(&sensorlink.SensorFrame{ChannelID: 3, Seq: 100}) {
		t.Fatal("full FIFO accepted another frame")
	}
	if len(q.pending) != sensorFIFOFrames+2 {
		t.Fatalf("unbounded pending frames: %d", len(q.pending))
	}
	q.finish()
	for seq := range uint32(sensorFIFOFrames) {
		f, ok := q.next(context.Background())
		if !ok || f.ChannelID != 3+seq%2 || f.Seq != seq {
			t.Fatalf("FIFO reordered: %+v, open=%v", f, ok)
		}
	}
	for _, ch := range []uint32{1, 2} {
		f, ok := q.next(context.Background())
		if !ok || f.ChannelID != ch || f.Seq != 99 {
			t.Fatalf("camera slot: %+v, open=%v", f, ok)
		}
	}
	if _, ok := q.next(context.Background()); ok {
		t.Fatal("closed queue did not drain")
	}
}

func TestCameraFrameQueueConcurrentDelivery(t *testing.T) {
	q := newCameraFrameQueue([]uint32{1, 2})
	var producers sync.WaitGroup
	for _, ch := range []uint32{1, 2} {
		producers.Go(func() {
			for seq := range uint32(1000) {
				q.offer(&sensorlink.SensorFrame{ChannelID: ch, Seq: seq})
			}
		})
	}
	go func() { producers.Wait(); q.finish() }()
	last := map[uint32]uint32{}
	for {
		f, ok := q.next(context.Background())
		if !ok {
			break
		}
		if previous, seen := last[f.ChannelID]; seen && f.Seq <= previous {
			t.Fatalf("camera went backwards: previous=%d current=%d", previous, f.Seq)
		}
		last[f.ChannelID] = f.Seq
	}
	if last[1] != 999 || last[2] != 999 {
		t.Fatalf("lost final frames: %v", last)
	}
}

func TestCameraFrameQueueWakesOnCloseAndCancellation(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			q := newCameraFrameQueue([]uint32{1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan bool, 1)
			go func() { _, ok := q.next(ctx); result <- ok }()
			synctest.Wait()
			if cancelContext {
				cancel()
			} else {
				q.finish()
				q.finish()
				q.offer(&sensorlink.SensorFrame{ChannelID: 1})
			}
			if <-result {
				t.Fatal("received a frame after termination")
			}
		})
	}
}

func TestLiveTransportRetainsNewestCompleteCameraFrame(t *testing.T) {
	for _, transport := range []string{"wendycom", "grpc", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var tr SensorTransport
				var send func(*sensorlinkpb.SensorData)
				var disconnected func()
				if transport == "wendycom" {
					client := newFakeWendycomClient()
					tr, _ = newTestWendycomTransport(zap.NewNop(), client)
					send, disconnected = client.send, client.dropOff
				} else if transport == "tcp" {
					local, remote := net.Pipe()
					tr = NewTCPTransport(pipeDialer{local}, "pipe")
					defer remote.Close()
					go func() {
						if err := sensorlink.WriteMessage(remote, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Manifest{Manifest: &sensorlinkpb.SensorManifest{}}}); err != nil {
							t.Error(err)
							return
						}
						if _, err := sensorlink.ReadMessage(remote); err != nil {
							t.Error(err)
						}
					}()
					send = func(d *sensorlinkpb.SensorData) {
						if err := sensorlink.WriteMessage(remote, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Data{Data: d}}); err != nil {
							t.Fatal(err)
						}
					}
					disconnected = func() { remote.Close() }
				} else {
					grpcTransport, client, _ := newObservedGRPCTransport()
					tr = grpcTransport
					send = func(d *sensorlinkpb.SensorData) { client.frames <- d }
					disconnected = func() { close(client.frames) }
				}
				next, closeStream, err := streamLiveFrames(context.Background(), tr, []uint32{1, 2, 3}, []uint32{1, 2})
				if err != nil {
					t.Fatal(err)
				}
				defer closeStream()
				send(wholeFrame(1, 0))
				send(wholeFrame(2, 0))
				send(wholeFrame(3, 0))
				for seq := uint32(1); seq < 100; seq++ {
					send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: seq, Payload: []byte("first")})
					send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: seq, ChunkSeq: 1, Flags: sensorlink.FlagLastChunk, Payload: []byte("last")})
				}
				// An unfinished newer frame must not replace the last complete one.
				send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 100, Payload: []byte("incomplete")})
				synctest.Wait()
				f, ok := next()
				if !ok || f.ChannelID != 1 || f.Seq != 99 || string(f.Payload) != "firstlast" {
					t.Fatalf("stale or partial frame after stalled consumer: %+v", f)
				}
				for _, ch := range []uint32{2, 3} {
					f, ok := next()
					if !ok || f.ChannelID != ch || f.Seq != 0 {
						t.Fatalf("another channel was evicted: %+v", f)
					}
				}
				disconnected()
				if _, ok := next(); ok {
					t.Fatal("disconnected stream stayed open")
				}
			})
		})
	}
}

type liveSelectionTransport struct {
	selectionTransport
	latest []uint32
}

func (tr *liveSelectionTransport) streamFrames(_ context.Context, channels []uint32, sink frameSink) (func() error, error) {
	for id := range sink.(*cameraFrameQueue).latest {
		tr.latest = append(tr.latest, id)
	}
	for _, id := range channels {
		sink.offer(&sensorlink.SensorFrame{ChannelID: id, Payload: []byte("frame")})
	}
	sink.finish()
	return func() error { return nil }, nil
}

func TestSupervisorUsesLatestOnlyForMountedMJPEG(t *testing.T) {
	video := func(id uint32, codec sensorlinkpb.VideoFormat_Codec) *sensorlinkpb.SensorDescriptor {
		return &sensorlinkpb.SensorDescriptor{ChannelId: id, Name: string(rune('a' + id)),
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: codec}}}
	}
	tr := &liveSelectionTransport{selectionTransport: selectionTransport{manifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{
		video(1, sensorlinkpb.VideoFormat_MJPEG), video(2, sensorlinkpb.VideoFormat_H264), video(3, sensorlinkpb.VideoFormat_MJPEG),
		{ChannelId: 4, Name: "mic", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 16000, Channels: 1}}},
	}}}}
	cam, audio := &selectionCamera{}, &selectionAudio{}
	s := NewSupervisor(zap.NewNop(), cam, func(SensorPairing, string) (SensorTransport, error) { return tr, nil },
		func(string) ros2camera.CameraWriter { return cam }, audio)
	delivered, err := s.streamOnce(context.Background(), SensorPairing{SourceAssetID: 1, Transport: "grpc", SensorAllowlist: []string{"b", "c", "mic"}}, "source")
	if err != nil || !delivered || !slices.Equal(tr.latest, []uint32{1}) || cam.frames != 2 || audio.frames != 1 {
		t.Fatalf("delivered=%v err=%v latest=%v camera=%d audio=%d", delivered, err, tr.latest, cam.frames, audio.frames)
	}
}
