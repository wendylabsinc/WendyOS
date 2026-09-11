package mcusource

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
)

func TestGRPCTransportBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, client, logs := newObservedGRPCTransport()
		frames, closeStream, err := tr.Stream(context.Background(), []uint32{1, 2})
		if err != nil {
			t.Fatal(err)
		}
		defer closeStream()

		// A stalled consumer must not block the receiver. The first eight
		// frames are queued; three more are dropped with just one warning.
		for seq := range uint32(11) {
			client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: seq}
		}
		synctest.Wait()
		assertDropLogs(t, logs, [][2]uint64{{1, 1}})
		fields := logs.All()[0].ContextMap()
		if fields["source"] != int32(7) || fields["addr"] != "sensor:50052" {
			t.Fatalf("missing source context: %v", fields)
		}
		channels, ok := fields["channels"].([]any)
		if !ok || len(channels) != 2 || channels[0] != uint32(1) || channels[1] != uint32(2) {
			t.Fatalf("missing subscribed channels: %v", fields)
		}

		// Continued congestion produces an aggregate warning after five
		// seconds, including the drops suppressed since the first warning.
		time.Sleep(5 * time.Second)
		client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 2, Seq: 11}
		synctest.Wait()
		assertDropLogs(t, logs, [][2]uint64{{1, 1}, {3, 4}})

		for seq := range uint32(8) {
			if f := <-frames; f.Seq != seq {
				t.Fatalf("queued frame seq = %d, want %d", f.Seq, seq)
			}
		}
		client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: 12}
		if f := <-frames; f.Seq != 12 {
			t.Fatalf("stream did not recover: got seq %d", f.Seq)
		}
		close(client.frames)
		synctest.Wait()
		if _, ok := <-frames; ok {
			t.Fatal("frames channel did not close")
		}
		assertDropLogs(t, logs, [][2]uint64{{1, 1}, {3, 4}})
	})
}

func TestGRPCTransportFlushesDropsOnExit(t *testing.T) {
	for _, end := range []string{"EOF", "receive error", "context cancellation", "stream close"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tr, client, logs := newObservedGRPCTransport()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				frames, closeStream, err := tr.Stream(ctx, []uint32{1})
				if err != nil {
					t.Fatal(err)
				}
				defer closeStream()
				for seq := range uint32(12) {
					client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: seq}
				}
				synctest.Wait()
				assertDropLogs(t, logs, [][2]uint64{{1, 1}})

				switch end {
				case "EOF":
					close(client.frames)
				case "receive error":
					client.err = errors.New("source disconnected")
					close(client.frames)
				case "context cancellation":
					cancel()
				case "stream close":
					if err := closeStream(); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				assertDropLogs(t, logs, [][2]uint64{{1, 1}, {3, 4}})
				var received int
				for range frames {
					received++
				}
				if received != 8 {
					t.Fatalf("received %d frames, want 8", received)
				}
			})
		})
	}
}

func TestGRPCTransportNoDropsNoWarnings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, client, logs := newObservedGRPCTransport()
		frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
		if err != nil {
			t.Fatal(err)
		}
		defer closeStream()
		for seq := range uint32(12) {
			client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: seq}
			if f := <-frames; f.Seq != seq {
				t.Fatalf("frame seq = %d, want %d", f.Seq, seq)
			}
		}
		close(client.frames)
		synctest.Wait()
		assertDropLogs(t, logs, nil)
	})
}

func TestGRPCTransportReportsPendingDropsAfterRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, client, logs := newObservedGRPCTransport()
		frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
		if err != nil {
			t.Fatal(err)
		}
		defer closeStream()
		for seq := range uint32(10) {
			client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: seq}
		}
		synctest.Wait()
		assertDropLogs(t, logs, [][2]uint64{{1, 1}})
		for range 8 {
			<-frames
		}
		time.Sleep(5 * time.Second)
		client.frames <- &sensorlinkpb.SensorFrame{ChannelId: 1, Seq: 10}
		if f := <-frames; f.Seq != 10 {
			t.Fatalf("stream did not recover: got seq %d", f.Seq)
		}
		synctest.Wait()
		assertDropLogs(t, logs, [][2]uint64{{1, 1}, {1, 2}})
		close(client.frames)
		synctest.Wait()
		assertDropLogs(t, logs, [][2]uint64{{1, 1}, {1, 2}})
	})
}

func assertDropLogs(t *testing.T, logs *observer.ObservedLogs, want [][2]uint64) {
	t.Helper()
	entries := logs.All()
	if len(entries) != len(want) {
		t.Fatalf("got %d warnings, want %d: %v", len(entries), len(want), entries)
	}
	for i, entry := range entries {
		fields := entry.ContextMap()
		if entry.Level != zap.WarnLevel || fields["dropped"] != want[i][0] || fields["dropped_total"] != want[i][1] {
			t.Fatalf("warning %d = %v, want dropped=%d dropped_total=%d", i, entry, want[i][0], want[i][1])
		}
	}
}

// An unbuffered fake gRPC receiver lets synctest settle the transport after
// each burst without relying on network timing or real sleeps.
type backpressureSensorClient struct {
	agentpbv2.WendySensorServiceClient
	frames chan *sensorlinkpb.SensorFrame
	err    error
}

func (c *backpressureSensorClient) StreamSensors(ctx context.Context, _ *agentpbv2.StreamSensorsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[sensorlinkpb.SensorFrame], error) {
	return &backpressureSensorStream{ctx: ctx, client: c}, nil
}

type backpressureSensorStream struct {
	grpc.ClientStream
	ctx    context.Context
	client *backpressureSensorClient
}

func (s *backpressureSensorStream) Recv() (*sensorlinkpb.SensorFrame, error) {
	select {
	case f, ok := <-s.client.frames:
		if ok {
			return f, nil
		}
		if s.client.err != nil {
			return nil, s.client.err
		}
		return nil, io.EOF
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func newObservedGRPCTransport() (*grpcTransport, *backpressureSensorClient, *observer.ObservedLogs) {
	core, logs := observer.New(zap.WarnLevel)
	client := &backpressureSensorClient{frames: make(chan *sensorlinkpb.SensorFrame)}
	logger := zap.New(core).With(zap.Int32("source", 7), zap.String("addr", "sensor:50052"))
	return &grpcTransport{logger: logger, client: client}, client, logs
}
