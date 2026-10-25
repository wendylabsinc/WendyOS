package lite

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"google.golang.org/protobuf/proto"
)

type fakeEntropy struct{}

func (fakeEntropy) RandomBytes(ctx context.Context, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return bytes.Repeat([]byte{7}, n), nil
}

type fakeLink struct {
	replies chan []byte
	done    chan struct{}
	once    sync.Once
	respond func(*pb.WendyComMessage) (*pb.WendyComMessage, error)
}

func newFakeLink() *fakeLink {
	return &fakeLink{replies: make(chan []byte, 16), done: make(chan struct{})}
}
func (l *fakeLink) Handshake() error { return nil }
func (l *fakeLink) Send(data []byte) error {
	m := new(pb.WendyComMessage)
	if err := proto.Unmarshal(data, m); err != nil {
		return err
	}
	var reply *pb.WendyComMessage
	if h := m.GetHandshake(); h != nil {
		reply = &pb.WendyComMessage{Msg: &pb.WendyComMessage_Handshake{Handshake: h}}
	} else if l.respond != nil {
		var err error
		reply, err = l.respond(m)
		if err != nil {
			return err
		}
	}
	if reply != nil {
		wire, err := proto.Marshal(reply)
		if err != nil {
			return err
		}
		select {
		case l.replies <- wire:
		case <-l.done:
			return io.EOF
		}
	}
	return nil
}
func (l *fakeLink) Receive(timeout time.Duration) ([]byte, error) {
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case data := <-l.replies:
		return data, nil
	case <-l.done:
		return nil, io.EOF
	case <-timer:
		return nil, context.DeadlineExceeded
	}
}
func (l *fakeLink) PreferredChunkSize() int { return 3 }
func (l *fakeLink) Close() error            { l.once.Do(func() { close(l.done) }); return nil }
func connectedFake(t *testing.T) (*Client, *fakeLink) {
	t.Helper()
	l := newFakeLink()
	c := NewClient(fakeEntropy{})
	if err := c.Connect(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, l
}
func response(cmd *pb.WendyComCommand, result pb.WendyComResult) *pb.WendyComMessage {
	return &pb.WendyComMessage{Msg: &pb.WendyComMessage_Response{Response: &pb.WendyComResponse{RequestId: cmd.RequestId, Result: result}}}
}

func TestClientLifecycleAndConnectionLoss(t *testing.T) {
	c, l := connectedFake(t)
	l.Close()
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("connection loss not signaled")
	}
	if err := c.Ping(); !errors.Is(err, io.EOF) {
		t.Fatalf("lost connection: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background(), newFakeLink()); err == nil {
		t.Fatal("reused terminal client")
	}
}
func TestClientCancellationInterruptsHandshake(t *testing.T) {
	l := newFakeLink()
	l.respond = func(*pb.WendyComMessage) (*pb.WendyComMessage, error) { return nil, nil }
	// Drain the loopback handshake so Receive remains blocked.
	blocked := &blockedHandshakeLink{fakeLink: l, entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewClient(fakeEntropy{}).Connect(ctx, blocked) }()
	<-blocked.entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake stayed blocked")
	}
	select {
	case <-l.done:
	default:
		t.Fatal("canceled link retained")
	}
}

type blockedHandshakeLink struct {
	*fakeLink
	entered chan struct{}
}

func (l *blockedHandshakeLink) Send([]byte) error { close(l.entered); return nil }

func TestPushAppPreservesChunkOffsetsAndAcknowledgement(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rejected"}[reject], func(t *testing.T) {
			c, l := connectedFake(t)
			var payload []byte
			var commands int
			l.respond = func(m *pb.WendyComMessage) (*pb.WendyComMessage, error) {
				cmd := m.GetCommand()
				commands++
				if b := cmd.GetAppPushBegin(); b != nil && b.Size != 7 {
					t.Errorf("size=%d", b.Size)
				}
				if d := cmd.GetAppPushData(); d != nil {
					if d.Offset != uint32(len(payload)) {
						t.Errorf("offset=%d", d.Offset)
					}
					payload = append(payload, d.Data...)
				}
				r := pb.WendyComResult_WENDY_COM_RESULT_OK
				if reject {
					r = pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE
				}
				return response(cmd, r), nil
			}
			var written uint32
			err := c.PushApp(strings.NewReader("abcdefg"), 7, AppTypeNative, func(n, total uint32) {
				written = n
				if total != 7 {
					t.Errorf("total=%d", total)
				}
			})
			if reject {
				if err == nil || commands != 1 {
					t.Fatalf("rejection=%v commands=%d", err, commands)
				}
			} else if err != nil || string(payload) != "abcdefg" || written != 7 || commands != 5 {
				t.Fatalf("payload=%q written=%d commands=%d error=%v", payload, written, commands, err)
			}
		})
	}
}
func TestResetEnrollmentRequiresFirmwareAcknowledgement(t *testing.T) {
	for _, result := range []pb.WendyComResult{pb.WendyComResult_WENDY_COM_RESULT_OK, pb.WendyComResult_WENDY_COM_RESULT_BAD_STATE} {
		t.Run(result.String(), func(t *testing.T) {
			c, l := connectedFake(t)
			l.respond = func(m *pb.WendyComMessage) (*pb.WendyComMessage, error) {
				cmd := m.GetCommand()
				p := cmd.GetEnrollmentReset()
				if p.GetExpectedDeviceId() != "441bf6804824" || p.GetNonceHex() != strings.Repeat("12", 32) {
					t.Error("reset lost hardware ID or nonce")
				}
				return response(cmd, result), nil
			}
			err := c.ResetEnrollment("441bf6804824", strings.Repeat("12", 32))
			if (err == nil) != (result == pb.WendyComResult_WENDY_COM_RESULT_OK) {
				t.Fatal(err)
			}
		})
	}
}
func TestResetEnrollmentRejectsInvalidBindingBeforeSending(t *testing.T) {
	for _, tc := range []struct{ id, nonce string }{{"", strings.Repeat("12", 32)}, {"device", ""}, {"device", strings.Repeat("gg", 32)}, {"device", strings.Repeat("12", 31)}} {
		if err := NewClient(fakeEntropy{}).ResetEnrollment(tc.id, tc.nonce); err == nil {
			t.Fatal("accepted invalid reset")
		}
	}
}
