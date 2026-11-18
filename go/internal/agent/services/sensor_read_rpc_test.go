package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestSensorReadRPCStreamDisconnectAndNoControlPlane(t *testing.T) {
	feed := NewGenericSensorFeed()
	_, publish, release := feed.Mount(mcusource.SensorPairing{SourceAssetID: 1}, []*sensorpb.SensorDescriptor{{ChannelId: 1, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "test", SampleBytes: 2}}}})
	defer release()
	server := NewSensorReadServer(feed)
	if len(server.GetServiceInfo()) != 1 {
		t.Fatal("read socket exposes unrelated services")
	}
	lis := bufconn.Listen(65536)
	defer lis.Close()
	go server.Serve(lis)
	defer server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///sensor-read", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agentpb.NewWendySensorServiceClient(conn)
	var headers metadata.MD
	m, err := client.GetSensorManifest(ctx, &agentpb.GetSensorManifestRequest{}, grpc.Header(&headers))
	if err != nil || len(m.GetSensors()) != 1 {
		t.Fatalf("manifest %v %v", m, err)
	}
	var origins map[uint32]SensorOrigin
	values := headers.Get(SensorOriginsHeader)
	if len(values) != 1 {
		t.Fatalf("missing origins: %v", headers)
	}
	if err := json.Unmarshal([]byte(values[0]), &origins); err != nil {
		t.Fatal(err)
	}
	if got := origins[m.Sensors[0].ChannelId]; got.Source != "asset:1" || got.ChannelID != 1 {
		t.Fatalf("wrong origin: %+v", got)
	}
	for _, tc := range []struct {
		ids  []uint32
		code codes.Code
	}{
		{nil, codes.InvalidArgument}, {make([]uint32, 65), codes.InvalidArgument}, {[]uint32{9999}, codes.NotFound},
	} {
		rejected, err := client.StreamSensors(ctx, &agentpb.StreamSensorsRequest{ChannelId: tc.ids})
		if err == nil {
			_, err = rejected.Recv()
		}
		if status.Code(err) != tc.code {
			t.Fatalf("selection error=%v, want=%v", err, tc.code)
		}
	}
	// A new subscriber must not receive an older captured sample.
	publish(&sensorlink.SensorFrame{ChannelID: 1, Payload: []byte{9, 9}})
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	stream, err := client.StreamSensors(readerCtx, &agentpb.StreamSensorsRequest{ChannelId: []uint32{m.Sensors[0].ChannelId}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.StreamSensors(ctx, &agentpb.StreamSensorsRequest{ChannelId: []uint32{m.Sensors[0].ChannelId}})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until both RPCs subscribe, then publish one complete sample.
	for {
		feed.mu.Lock()
		n := len(feed.subscribers)
		feed.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	publish(&sensorlink.SensorFrame{ChannelID: 1, TsUs: 1, Payload: []byte{3, 4}})
	sample, err := stream.Recv()
	if err != nil || string(sample.GetPayload()) != string([]byte{3, 4}) {
		t.Fatalf("frame %v %v", sample, err)
	}
	other, err := second.Recv()
	if err != nil || string(other.GetPayload()) != string(sample.GetPayload()) {
		t.Fatalf("fanout: %v %v", other, err)
	}
	cancelReader()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("reader cancellation: %v", err)
	}
	publish(&sensorlink.SensorFrame{ChannelID: 1, Seq: 42, TsUs: 0, Payload: []byte{5, 6}})
	other, err = second.Recv()
	if err != nil || other.FrameSeq != 42 || other.TsUs != 0 || string(other.Payload) != string([]byte{5, 6}) {
		t.Fatalf("one reader cancelled another: %v %v", other, err)
	}
	release()
	if _, err := second.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("unmount must end stream: %v", err)
	}
	if err := conn.Invoke(ctx, "/wendy.agent.services.v2.WendyContainerService/StopContainer", &agentpb.GetSensorManifestRequest{}, &agentpb.GetSensorManifestRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("control method accessible: %v", err)
	}
}

// A blocked transport Send must not keep a disconnected or overflowing reader
// registered forever. Real gRPC cancels the send when the handler returns.
type blockedSensorStream struct {
	grpc.ServerStream
	ctx     context.Context
	entered chan struct{}
	exited  chan struct{}
}

func (s *blockedSensorStream) Context() context.Context { return s.ctx }
func (s *blockedSensorStream) Send(*sensorpb.SensorData) error {
	close(s.entered)
	<-s.ctx.Done()
	close(s.exited)
	return s.ctx.Err()
}

func TestSensorReadBlockedSendTerminates(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint("disconnect=", disconnect), func(t *testing.T) {
			feed := NewGenericSensorFeed()
			_, publish, release := feed.Mount(mcusource.SensorPairing{SourceAssetID: 1}, []*sensorpb.SensorDescriptor{{ChannelId: 1, Format: &sensorpb.SensorDescriptor_Sensor{Sensor: &sensorpb.SensorFormat{Schema: "example", SampleBytes: 1}}}})
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream := &blockedSensorStream{ctx: ctx, entered: make(chan struct{}), exited: make(chan struct{})}
			finished := make(chan error, 1)
			go func() { finished <- feed.StreamSensors(&agentpb.StreamSensorsRequest{ChannelId: []uint32{1}}, stream) }()
			for {
				feed.mu.Lock()
				n := len(feed.subscribers)
				feed.mu.Unlock()
				if n == 1 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("reader did not register")
				case <-time.After(time.Millisecond):
				}
			}
			publish(&sensorlink.SensorFrame{ChannelID: 1, Payload: []byte{1}})
			select {
			case <-stream.entered:
			case <-ctx.Done():
				t.Fatal("send did not start")
			}
			want := codes.ResourceExhausted
			if disconnect {
				release()
				want = codes.Unavailable
			} else {
				for i := 0; i < 9; i++ {
					publish(&sensorlink.SensorFrame{ChannelID: 1, Payload: []byte{1}})
				}
			}
			select {
			case err := <-finished:
				if status.Code(err) != want {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-ctx.Done():
				t.Fatal("blocked send prevented RPC termination")
			}
			cancel()
			select {
			case <-stream.exited:
			case <-time.After(time.Second):
				t.Fatal("send goroutine did not exit")
			}
			feed.mu.Lock()
			defer feed.mu.Unlock()
			if len(feed.subscribers) != 0 {
				t.Fatal("reader leaked")
			}
		})
	}
}
