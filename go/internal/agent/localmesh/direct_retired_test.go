package localmesh

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestRetiredDirectLaneKeepsCarrierDatagramsMoving(t *testing.T) {
	x, y := net.Pipe()
	a := newNegotiatedStreamNodeLink(x, 8, true)
	b := newNegotiatedStreamNodeLink(y, 8, true)
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		done <- serveDirectCarrier(ctx, DirectCarrierAdmission{Records: b}, func(context.Context, DirectCarrierAdmission) error {
			close(entered)
			return ErrDirectLaneRetired
		})
	}()
	<-entered
	// More than the receiver queue capacity: an abandoned direct lane would
	// prevent the subsequent routing packet from reaching the mesh stack.
	for i := 0; i < 20; i++ {
		for {
			ok, err := a.TrySendDirectRecord([]byte{1, byte(i)})
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	// Write directly into the same ordered priority queue so the packet cannot
	// leapfrog the stale records through the normal routing-priority scheduler.
	select {
	case a.priority <- streamFrame{kind: streamPacket, data: []byte{PacketBabel, 42}}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	got, err := b.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, []byte{PacketBabel, 42}) {
		t.Fatal(got, err)
	}
	select {
	case err := <-done:
		t.Fatal("carrier retired with lane", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retired reader not joined")
	}
}

func TestOrdinaryDirectHandlerReturnDoesNotRetireLane(t *testing.T) {
	want := errors.New("invalid promotion proof")
	if got := serveDirectCarrier(context.Background(), DirectCarrierAdmission{}, func(context.Context, DirectCarrierAdmission) error { return want }); got != want {
		t.Fatal(got)
	}
	if got := serveDirectCarrier(context.Background(), DirectCarrierAdmission{}, func(context.Context, DirectCarrierAdmission) error { return nil }); got != nil {
		t.Fatal(got)
	}
}
