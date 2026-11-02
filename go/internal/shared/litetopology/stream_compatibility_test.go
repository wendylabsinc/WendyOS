package litetopology

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func compatibilityHandshakePipe(t *testing.T, serve func(net.Conn, *lite.WendyComMessage) error) net.Conn {
	t.Helper()
	client, device := net.Pipe()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, err := compatibilityReadKnownFrame(device)
		if err == nil {
			err = serve(device, request)
		}
		// Keep a successful handshake transport live while the reader clears
		// its setup deadline. Cleanup also releases intentionally blocked sends.
		if err == nil {
			<-stop
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = client.Close()
		_ = device.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("WendyCom fixture retained its writer")
		}
	})
	return client
}

func TestStreamHandshakeRetainsKnownFrameAndAdmissionErrors(t *testing.T) {
	for _, kind := range []string{"wrong-id", "wrong-major", "command", "response", "service", "empty", "oversized", "bad-magic", "bad-version", "bad-protobuf"} {
		t.Run(kind, func(t *testing.T) {
			client := compatibilityHandshakePipe(t, func(c net.Conn, request *lite.WendyComMessage) error {
				switch kind {
				case "wrong-id":
					request.GetHandshake().HandshakeId++
				case "wrong-major":
					request.GetHandshake().Version.Major = 3
				case "command":
					request.Msg = &lite.WendyComMessage_Command{Command: &lite.WendyComCommand{}}
				case "response":
					request.Msg = &lite.WendyComMessage_Response{Response: &lite.WendyComResponse{}}
				case "service":
					request.Msg = &lite.WendyComMessage_Service{Service: &lite.WendyComService{}}
				case "bad-protobuf":
					return compatibilityRawFrame(c, 0, 0, []byte{0xff})
				default:
					header := []byte{0xa5, 2, 0, 0, 0, 0, 0, 0}
					switch kind {
					case "oversized":
						binary.BigEndian.PutUint16(header[6:], 4097)
					case "bad-magic":
						header[0], header[2] = 0xa4, 99
					case "bad-version":
						header[1], header[2] = 3, 99
					}
					_, err := c.Write(header)
					return err
				}
				return compatibilityKnownFrame(c, request)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := OpenStream(ctx, client); err == nil {
				t.Fatal("known admission or frame error became optional")
			} else if (kind == "wrong-id" || kind == "wrong-major" || kind == "command" || kind == "response" || kind == "service") && status.Code(err) != codes.Unimplemented {
				t.Fatal("known admission rejection changed:", err)
			}
		})
	}
}

func TestStreamExtensionsRetainCountAndDiscardByteBounds(t *testing.T) {
	for _, kind := range []string{"count", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			client := compatibilityHandshakePipe(t, func(c net.Conn, request *lite.WendyComMessage) error {
				count, body := 32, []byte(nil)
				if kind == "bytes" {
					count, body = 2, make([]byte, 65535)
				}
				for range count {
					if err := compatibilityRawFrame(c, 99, 0, body); err != nil {
						return err
					}
				}
				return compatibilityKnownFrame(c, request)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := OpenStream(ctx, client)
			want := codes.Unavailable
			if kind == "bytes" {
				want = codes.ResourceExhausted
			}
			if status.Code(err) != want {
				t.Fatalf("%s guard = %v, want %v", kind, err, want)
			}
		})
	}
}

type compatibilityFragmentConn struct{ net.Conn }

func (c compatibilityFragmentConn) Read(p []byte) (int, error) {
	return c.Conn.Read(p[:min(len(p), 17)])
}

func compatibilityPaddedMessage(t *testing.T, message *lite.WendyComMessage) []byte {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	body = protowire.AppendTag(body, 123, protowire.BytesType)
	body = protowire.AppendBytes(body, make([]byte, 4096-len(body)-2))
	if len(body) != 4096 {
		t.Fatal("fixture size:", len(body))
	}
	return body
}

func TestStreamDiscardBudgetExcludesMatchingDataAndResetsForPages(t *testing.T) {
	event := compatibilityPaddedMessage(t, &lite.WendyComMessage{Msg: &lite.WendyComMessage_Event{Event: &lite.WendyComEvent{}}})
	page := fixture(t)
	page.NextCursor = 0
	client := compatibilityHandshakePipe(t, func(c net.Conn, request *lite.WendyComMessage) error {
		for range 31 {
			if err := compatibilityRawFrame(c, 0, 0, event); err != nil {
				return err
			}
		}
		if err := compatibilityRawFrame(c, 0, 0, compatibilityPaddedMessage(t, request)); err != nil {
			return err
		}
		command, err := compatibilityReadKnownFrame(c)
		if err != nil {
			return err
		}
		if err := compatibilityRawFrame(c, 0, 77, make([]byte, 5000)); err != nil {
			return err
		}
		return compatibilityKnownFrame(c, &lite.WendyComMessage{Msg: &lite.WendyComMessage_Response{Response: &lite.WendyComResponse{RequestId: command.GetCommand().RequestId, Data: &lite.WendyComResponse_NetTopology{NetTopology: page}}}})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := ReadStream(ctx, compatibilityFragmentConn{client})
	if err != nil || out == nil || len(out.Peers) != 1 {
		t.Fatalf("matching known data used extension budget, or page budget failed to reset: %v %v", out, err)
	}
}

type compatibilityDeadlineConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *compatibilityDeadlineConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, d)
	c.mu.Unlock()
	return c.Conn.SetDeadline(d)
}

func TestStreamFutureTrafficKeepsOriginalDeadline(t *testing.T) {
	client := compatibilityHandshakePipe(t, func(c net.Conn, _ *lite.WendyComMessage) error {
		for {
			if err := compatibilityRawFrame(c, 99, 0, nil); err != nil {
				return err
			}
			time.Sleep(4 * time.Millisecond)
		}
	})
	conn := &compatibilityDeadlineConn{Conn: client}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := OpenStream(ctx, conn); err == nil {
		t.Fatal("continuous future traffic completed handshake")
	}
	if time.Since(start) > time.Second {
		t.Fatal("future traffic extended setup")
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	deadline, _ := ctx.Deadline()
	if len(conn.deadlines) != 1 || !conn.deadlines[0].Equal(deadline) {
		t.Fatal("future traffic reset original deadline:", conn.deadlines)
	}
}

func TestStreamCancellationReleasesPartialUnknownBody(t *testing.T) {
	bodyStarted := make(chan struct{})
	client := compatibilityHandshakePipe(t, func(c net.Conn, _ *lite.WendyComMessage) error {
		header := []byte{0xa5, 2, 77, 0, 0, 0, 0, 32, 1}
		if _, err := c.Write(header); err != nil {
			return err
		}
		close(bodyStarted)
		_, err := io.Copy(io.Discard, c)
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := OpenStream(ctx, client); done <- err }()
	select {
	case <-bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("unknown body did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled partial unknown frame succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation retained unknown body reader")
	}
}
