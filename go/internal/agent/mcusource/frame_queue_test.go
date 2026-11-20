package mcusource

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCameraFrameQueueKeepsFIFOAndNewestCameraFramesWhileStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newLiveFrameQueue([]uint32{1, 2}, nil)
		// A saturated audio/H.264 FIFO must not prevent either camera from
		// retaining its newest frame or let camera churn evict FIFO frames.
		for seq := range uint32(sensorFIFOFrames) {
			if q.offer(&sensorlink.SensorFrame{ChannelID: 3 + seq%2, Seq: seq}) != 0 {
				t.Fatal("early FIFO drop")
			}
		}
		// Nothing consumes during 3.3 seconds of 30 fps video.
		for seq := range uint32(100) {
			for _, ch := range []uint32{1, 2} {
				want := 1
				if seq == 0 {
					want = 0
				}
				if discarded := q.offer(&sensorlink.SensorFrame{ChannelID: ch, Seq: seq}); discarded != want {
					t.Fatalf("channel %d seq %d: discarded %d frames, want %d", ch, seq, discarded, want)
				}
			}
			time.Sleep(33 * time.Millisecond)
		}
		if q.offer(&sensorlink.SensorFrame{ChannelID: 3, Seq: 100}) != 1 {
			t.Fatal("full FIFO accepted another frame")
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
				t.Fatalf("camera did not resume at its newest frame: %+v, open=%v", f, ok)
			}
		}
		if _, ok := q.next(context.Background()); ok {
			t.Fatal("closed queue did not drain")
		}
	})
}

func TestCameraFrameQueueDeliversFramesThatArriveTogether(t *testing.T) {
	q := newLiveFrameQueue([]uint32{1}, nil)
	// A network stall releases several frames at once. The consumer is not
	// behind; it has simply not run yet.
	for seq := range uint32(5) {
		if discarded := q.offer(&sensorlink.SensorFrame{ChannelID: 1, Seq: seq}); discarded != 0 {
			t.Fatalf("seq %d: discarded %d frames that arrived together", seq, discarded)
		}
	}
	q.finish()
	for seq := range uint32(5) {
		f, ok := q.next(context.Background())
		if !ok || f.Seq != seq {
			t.Fatalf("got %+v, open=%v, want seq %d", f, ok, seq)
		}
	}
	if _, ok := q.next(context.Background()); ok {
		t.Fatal("closed queue did not drain")
	}
}

func TestCameraFrameQueueBoundsFramesThatArriveTogether(t *testing.T) {
	q := newLiveFrameQueue([]uint32{1}, nil)
	var discarded int
	for seq := range uint32(cameraRunFrames + 2) {
		discarded += q.offer(&sensorlink.SensorFrame{ChannelID: 1, Seq: seq})
	}
	if discarded != 2 {
		t.Fatalf("discarded %d frames, want the 2 oldest", discarded)
	}
	q.finish()
	for seq := uint32(2); seq < cameraRunFrames+2; seq++ {
		f, ok := q.next(context.Background())
		if !ok || f.Seq != seq {
			t.Fatalf("got %+v, open=%v, want seq %d", f, ok, seq)
		}
	}
	if _, ok := q.next(context.Background()); ok {
		t.Fatal("closed queue did not drain")
	}
}

func TestCameraFrameQueueSlowConsumerSharesTurnsOnNewestFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 33 * time.Millisecond
		const frames = 90
		q := newLiveFrameQueue([]uint32{1, 2}, nil)
		start := time.Now()
		go func() {
			for seq := range uint32(frames) {
				for _, ch := range []uint32{1, 2} {
					q.offer(&sensorlink.SensorFrame{ChannelID: ch, Seq: seq})
				}
				time.Sleep(interval)
			}
			q.finish()
		}()
		// Offset the consumer so it never runs at the instant a frame arrives.
		time.Sleep(500 * time.Microsecond)
		delivered := map[uint32]int{}
		for {
			f, ok := q.next(context.Background())
			if !ok {
				break
			}
			newest := min(uint32(time.Since(start)/interval), frames-1)
			if f.Seq != newest {
				t.Fatalf("camera %d delivered seq %d when seq %d had arrived", f.ChannelID, f.Seq, newest)
			}
			delivered[f.ChannelID]++
			time.Sleep(40 * time.Millisecond) // slower than either camera
		}
		// Discarding a stale frame must not cost a camera its turn.
		if difference := delivered[1] - delivered[2]; delivered[1] < 30 || difference < -1 || difference > 1 {
			t.Fatalf("cameras did not share the consumer: %v", delivered)
		}
	})
}

func TestCameraFrameQueueConcurrentDelivery(t *testing.T) {
	q := newLiveFrameQueue([]uint32{1, 2}, nil)
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
			q := newLiveFrameQueue([]uint32{1}, nil)
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

func TestLiveFrameQueueKeepsGenericAndMediaFIFOSeparate(t *testing.T) {
	q := newLiveFrameQueue(nil, map[uint32]uint32{3: 2, 4: 2})
	for range genericFIFOFrames {
		if q.offer(&sensorlink.SensorFrame{ChannelID: 3, Payload: []byte{1}}) != 0 {
			t.Fatal("malformed frame counted as a backpressure drop")
		}
	}
	if q.offer(&sensorlink.SensorFrame{ChannelID: 3, Payload: make([]byte, MaxGenericFrameBytes+2)}) != 0 {
		t.Fatal("oversized batch counted as a backpressure drop")
	}
	for seq := range uint32(genericFIFOFrames) {
		if q.offer(&sensorlink.SensorFrame{ChannelID: 3 + seq%2, Seq: seq, Payload: []byte{1, 2, 3, 4}}) != 0 {
			t.Fatal("early generic drop")
		}
	}
	if q.offer(&sensorlink.SensorFrame{ChannelID: 3, Seq: genericFIFOFrames, Payload: []byte{1, 2}}) != 1 {
		t.Fatal("full generic FIFO accepted another frame")
	}
	for seq := range uint32(sensorFIFOFrames) {
		if q.offer(&sensorlink.SensorFrame{ChannelID: 1 + seq%2, Seq: seq}) != 0 {
			t.Fatal("generic burst consumed media capacity")
		}
	}
	if q.offer(&sensorlink.SensorFrame{ChannelID: 1, Seq: sensorFIFOFrames}) != 1 {
		t.Fatal("full media FIFO accepted another frame")
	}
	q.finish()

	type frameID struct{ channel, seq uint32 }
	var got []frameID
	for {
		frame, ok := q.next(context.Background())
		if !ok {
			break
		}
		got = append(got, frameID{frame.ChannelID, frame.Seq})
	}
	var want []frameID
	for seq := range uint32(max(sensorFIFOFrames, genericFIFOFrames)) {
		if seq < sensorFIFOFrames {
			want = append(want, frameID{1 + seq%2, seq})
		}
		if seq < genericFIFOFrames {
			want = append(want, frameID{3 + seq%2, seq})
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("drained %+v, want %+v", got, want)
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
				next, closeStream, err := streamLiveFrames(context.Background(), tr, []uint32{1, 2, 3}, []uint32{1, 2}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer closeStream()
				send(wholeFrame(1, 0))
				send(wholeFrame(2, 0))
				send(wholeFrame(3, 0))
				// Nothing consumes during 3.3 seconds of 30 fps video.
				for seq := uint32(1); seq < 100; seq++ {
					time.Sleep(33 * time.Millisecond)
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

func TestLiveTransportReportsOnlyReplacedCameraFramesAsDrops(t *testing.T) {
	for _, transport := range []string{"wendycom", "grpc"} {
		t.Run(transport, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				core, logs := observer.New(zap.WarnLevel)
				var tr SensorTransport
				var send func(*sensorlinkpb.SensorData)
				if transport == "wendycom" {
					client := newFakeWendycomClient()
					tr, _ = newTestWendycomTransport(zap.New(core), client)
					send = client.send
				} else {
					client := &backpressureSensorClient{frames: make(chan *sensorlinkpb.SensorData)}
					tr = &grpcTransport{logger: zap.New(core), client: client}
					send = func(d *sensorlinkpb.SensorData) { client.frames <- d }
				}
				next, closeStream, err := streamLiveFrames(context.Background(), tr, []uint32{1}, []uint32{1}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer closeStream()
				// Frames that arrive together wait for the consumer.
				for seq := range uint32(3) {
					send(wholeFrame(1, seq))
				}
				synctest.Wait()
				assertDropLogs(t, logs, nil)
				// The consumer is still away when the next frame arrives.
				time.Sleep(33 * time.Millisecond)
				send(wholeFrame(1, 3))
				synctest.Wait()
				assertDropLogs(t, logs, [][2]uint64{{3, 3}})
				if f, ok := next(); !ok || f.Seq != 3 {
					t.Fatalf("got %+v, open=%v, want the newest frame", f, ok)
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
	for id := range sink.(*liveFrameQueue).latest {
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
