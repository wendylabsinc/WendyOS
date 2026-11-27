package cameraframe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func chunks(t *testing.T, f Frame) []*appv1.SensorFrameChunk {
	t.Helper()
	m, err := EncodeMetadata(f.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	var out []*appv1.SensorFrameChunk
	for off := 0; off < len(f.JPEG); off += MaxChunkBytes {
		end := min(off+MaxChunkBytes, len(f.JPEG))
		out = append(out, &appv1.SensorFrameChunk{MetadataJson: bytes.Clone(m), Offset: int64(off), Data: bytes.Clone(f.JPEG[off:end])})
	}
	return out
}

type fakeAPI struct {
	calls int
	open  func(context.Context) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error)
}

func (a *fakeAPI) GetSensorFrame(ctx context.Context, _ *appv1.GetSensorFrameRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error) {
	a.calls++
	return a.open(ctx)
}

type fakeStream struct {
	grpc.ClientStream
	next func() (*appv1.SensorFrameChunk, error)
}

func (s *fakeStream) Recv() (*appv1.SensorFrameChunk, error) { return s.next() }
func fixtureAPI(rows []*appv1.SensorFrameChunk, terminal error) *fakeAPI {
	return &fakeAPI{open: func(context.Context) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error) {
		i := 0
		return &fakeStream{next: func() (*appv1.SensorFrameChunk, error) {
			if i == len(rows) {
				return nil, terminal
			}
			c := rows[i]
			i++
			return c, nil
		}}, nil
	}}
}
func testClient(t *testing.T, a API, timeout time.Duration) *Client {
	t.Helper()
	c, err := New(a, Config{ComponentType: "wendy.sensor.image.v1", Stream: "camera", Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type frameServer struct {
	appv1.UnimplementedWorldAppServiceServer
	frame      Frame
	calls      atomic.Int32
	afterFirst func()
}

func (s *frameServer) GetSensorFrame(r *appv1.GetSensorFrameRequest, stream grpc.ServerStreamingServer[appv1.SensorFrameChunk]) error {
	s.calls.Add(1)
	if r.ComponentType != "wendy.sensor.image.v1" || r.Stream != "camera" {
		return status.Error(codes.PermissionDenied, "scope")
	}
	f := s.frame.Clone()
	m, err := EncodeMetadata(f.Metadata)
	if err != nil {
		return err
	}
	for off := 0; off < len(f.JPEG); off += MaxChunkBytes {
		end := min(off+MaxChunkBytes, len(f.JPEG))
		if err := stream.Send(&appv1.SensorFrameChunk{MetadataJson: m, Offset: int64(off), Data: f.JPEG[off:end]}); err != nil {
			return err
		}
		if off == 0 && s.afterFirst != nil {
			s.afterFirst()
		}
	}
	return nil
}

func TestClientActualGRPCSingleSnapshot(t *testing.T) {
	f := testFrame(t)
	s := &frameServer{frame: f.Clone()}
	s.afterFirst = func() { s.frame.JPEG[20] ^= 1; s.frame.Metadata.FrameSequence = 0 }
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	appv1.RegisterWorldAppServiceServer(server, s)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///frame", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	got, err := testClient(t, appv1.NewWorldAppServiceClient(conn), time.Second).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.JPEG, f.JPEG) || got.Metadata.FrameSequence != f.Metadata.FrameSequence || s.calls.Load() != 1 {
		t.Fatal("single original snapshot changed")
	}
}

func TestClientRejectsPartialOrChangedStreamsWithoutRetry(t *testing.T) {
	f := testFrame(t)
	cases := map[string]func([]*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk{
		"truncated": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk { return c[:len(c)-1] },
		"offset":    func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk { c[1].Offset++; return c },
		"metadata": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk {
			c[1].MetadataJson = append(c[1].MetadataJson, ' ')
			return c
		},
		"digest":         func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk { c[1].Data[20] ^= 1; return c },
		"short interior": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk { c[0].Data = c[0].Data[:1]; return c },
		"oversize": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk {
			c[0].Data = append(c[0].Data, 0)
			return c
		},
		"extra": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk {
			return append(c, &appv1.SensorFrameChunk{MetadataJson: c[0].MetadataJson, Offset: int64(len(f.JPEG)), Data: []byte{1}})
		},
		"unknown proto": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk {
			c[0].ProtoReflect().SetUnknown([]byte{0x20, 0x01})
			return c
		},
		"empty": func(c []*appv1.SensorFrameChunk) []*appv1.SensorFrameChunk { return nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			a := fixtureAPI(change(chunks(t, f)), io.EOF)
			got, err := testClient(t, a, time.Second).Read(context.Background())
			if !errors.Is(err, ErrFrame) || len(got.JPEG) != 0 || a.calls != 1 {
				t.Fatalf("accepted partial/retried: %v, %d", err, a.calls)
			}
		})
	}
	a := fixtureAPI(chunks(t, f), status.Error(codes.Unavailable, "snapshot refused"))
	got, err := testClient(t, a, time.Second).Read(context.Background())
	if status.Code(err) != codes.Unavailable || len(got.JPEG) != 0 || a.calls != 1 {
		t.Fatal("terminal refusal hidden or retried")
	}
}

func TestClientCancellationTimeoutAndConfig(t *testing.T) {
	a := &fakeAPI{open: func(ctx context.Context) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error) {
		return &fakeStream{next: func() (*appv1.SensorFrameChunk, error) { <-ctx.Done(); return nil, ctx.Err() }}, nil
	}}
	c := testClient(t, a, 10*time.Millisecond)
	if _, err := c.Read(context.Background()); !errors.Is(err, context.DeadlineExceeded) || a.calls != 1 {
		t.Fatal("whole stream timeout", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Read(ctx); !errors.Is(err, context.Canceled) || a.calls != 1 {
		t.Fatal("canceled read reached server", err)
	}
	f := testFrame(t)
	a = fixtureAPI(chunks(t, f), io.EOF)
	ctx, cancel = context.WithCancel(context.Background())
	base := a.open
	a.open = func(c context.Context) (grpc.ServerStreamingClient[appv1.SensorFrameChunk], error) {
		s, e := base(c)
		return &fakeStream{next: func() (*appv1.SensorFrameChunk, error) {
			v, e := s.Recv()
			if errors.Is(e, io.EOF) {
				cancel()
			}
			return v, e
		}}, e
	}
	if got, err := testClient(t, a, time.Second).Read(ctx); !errors.Is(err, context.Canceled) || len(got.JPEG) != 0 {
		t.Fatal("final cancellation hidden", err)
	}
	for _, cfg := range []Config{{Stream: "camera"}, {ComponentType: "image", Stream: "bad stream"}, {ComponentType: "image", Stream: "camera", Timeout: 31 * time.Second}} {
		if _, err := New(a, cfg); !errors.Is(err, ErrConfig) {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestClientOwnsReceivedBuffers(t *testing.T) {
	f := testFrame(t)
	rows := chunks(t, f)
	a := fixtureAPI(rows, io.EOF)
	got, err := testClient(t, a, time.Second).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		row.Data[0] ^= 1
		row.MetadataJson[0] = 'x'
		proto.Reset(row)
	}
	if err := got.Validate(); err != nil {
		t.Fatal("stream buffers alias result", err)
	}
}
