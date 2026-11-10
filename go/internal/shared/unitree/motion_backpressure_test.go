package unitree

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type motionChannelFixture struct {
	mu       sync.Mutex
	buffered uint64
	sent     []string
	onSend   func(string)
}

func (c *motionChannelFixture) BufferedAmount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffered
}
func (c *motionChannelFixture) SendText(raw string) error {
	c.mu.Lock()
	c.sent = append(c.sent, raw)
	c.buffered += uint64(len(raw))
	c.mu.Unlock()
	if c.onSend != nil {
		c.onSend(raw)
	}
	return nil
}
func newMotionFixture() (*Connection, *motionChannelFixture) {
	ch := new(motionChannelFixture)
	c := &Connection{channel: ch, done: make(chan struct{})}
	c.connected.Store(true)
	return c, ch
}
func TestMoveDoesNotWaitForDelayedTransportAcknowledgement(t *testing.T) {
	c, ch := newMotionFixture()
	// A previous command reached the robot but its SCTP ACK is delayed.
	ch.buffered = 180
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := c.Move(ctx, 0, 0, .8); err != nil {
		t.Fatal("delivered-but-unacknowledged bytes blocked fresh movement", err)
	}
	if len(ch.sent) != 1 || ch.buffered > maxMotionBufferedBytes {
		t.Fatal("invalid motion pipeline")
	}
}
func TestMovePipelineRemainsBoundedAndStopBypassesBackpressure(t *testing.T) {
	c, ch := newMotionFixture()
	for i := 0; i < 10; i++ {
		_, _, err := c.sendSportRequest(context.Background(), 1008, `{"x":0,"y":0,"z":0.8}`)
		if errors.Is(err, errCommandBacklog) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(ch.sent) < 2 || len(ch.sent) > 3 || ch.buffered > maxMotionBufferedBytes {
		t.Fatal("unbounded or serialized pipeline", len(ch.sent), ch.buffered)
	}
	sent := len(ch.sent)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Move(ctx, 0, 0, .8); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("congested update did not time out", err)
	}
	if len(ch.sent) != sent {
		t.Fatal("queued a stale move while congested")
	}
	ch.onSend = func(raw string) {
		var request struct {
			Data struct {
				Header struct {
					Identity struct {
						ID  uint64 `json:"id"`
						API uint64 `json:"api_id"`
					} `json:"identity"`
				} `json:"header"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatal(err)
		}
		if request.Data.Header.Identity.API != 1003 {
			t.Fatal("unexpected command")
		}
		c.responseMu.Lock()
		pending := c.pending[request.Data.Header.Identity.ID]
		c.responseMu.Unlock()
		pending.response <- 0
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal("backpressure blocked stop", err)
	}
	if len(ch.sent) != sent+1 {
		t.Fatal("stop was not sent")
	}
}
