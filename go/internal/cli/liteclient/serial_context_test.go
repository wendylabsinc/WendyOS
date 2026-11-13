package liteclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"go.bug.st/serial"
	"google.golang.org/protobuf/proto"
)

type cancellingSerialPort struct {
	serial.Port
	mu            sync.Mutex
	echo          bool
	closed        bool
	inWendyCom    bool
	buffer        []byte
	events        []string
	lateCalls     int
	started       chan struct{}
	startOnce     sync.Once
	closedCh      chan struct{}
	cancelAtReply context.CancelFunc
}

func (p *cancellingSerialPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.lateCalls++
		return 0, io.ErrClosedPipe
	}
	switch {
	case bytes.Equal(data, []byte{escapeChar, 'o'}):
		p.events = append(p.events, "exit")
	case bytes.Equal(data, []byte{escapeChar, 'm'}):
		p.inWendyCom = true
		p.events = append(p.events, "mode")
	case bytes.Equal(data, []byte{escapeChar, keepAliveCmd}):
		p.events = append(p.events, "keepalive")
	case !p.inWendyCom:
		p.events = append(p.events, "sentinel")
		if p.echo {
			p.buffer = append(p.buffer, data[len(data)-32:]...)
		}
	default:
		p.events = append(p.events, "handshake")
		if p.cancelAtReply != nil {
			frame := bytes.ReplaceAll(data, []byte{escapeChar, '_'}, []byte{escapeChar})
			message := &wendypb.WendyComMessage{}
			if err := proto.Unmarshal(frame[headerSize:], message); err != nil {
				return 0, err
			}
			body, err := proto.Marshal(message)
			if err != nil {
				return 0, err
			}
			header := make([]byte, headerSize)
			header[0], header[1] = headerMagic, headerVersion
			binary.BigEndian.PutUint16(header[6:], uint16(len(body)))
			p.buffer = append(header, body...)
		}
	}
	return len(data), nil
}

func (p *cancellingSerialPort) Read(dst []byte) (int, error) {
	p.mu.Lock()
	if len(p.buffer) > 0 {
		n := copy(dst, p.buffer)
		p.buffer = p.buffer[n:]
		cancel := p.cancelAtReply
		shouldCancel := p.inWendyCom && len(p.buffer) == 0 && cancel != nil
		p.mu.Unlock()
		if shouldCancel {
			cancel()
		}
		return n, nil
	}
	p.mu.Unlock()
	p.startOnce.Do(func() { close(p.started) })
	<-p.closedCh
	return 0, io.ErrClosedPipe
}

func (p *cancellingSerialPort) SetReadTimeout(time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.lateCalls++
		return io.ErrClosedPipe
	}
	return nil
}

func (p *cancellingSerialPort) Drain() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.lateCalls++
		return io.ErrClosedPipe
	}
	p.events = append(p.events, "drain")
	return nil
}

func (p *cancellingSerialPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.lateCalls++
		return io.ErrClosedPipe
	}
	p.events = append(p.events, "close")
	p.closed = true
	close(p.closedCh)
	return nil
}

func (p *cancellingSerialPort) ResetOutputBuffer() error { return nil }

func TestSerialCancellationClosesLinkInOrderOnce(t *testing.T) {
	for _, phase := range []string{"serial handshake", "WendyCom handshake", "successful handshake"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			port := &cancellingSerialPort{
				echo: phase != "serial handshake", started: make(chan struct{}), closedCh: make(chan struct{}),
			}
			if phase == "successful handshake" {
				port.cancelAtReply = cancel
			}
			client := NewWendyLiteClient()
			link := newSerialLink(port)
			client.link = link
			completed := make(chan error, 1)
			go func() { completed <- client.handshakeSerialContext(ctx, link) }()
			if phase != "successful handshake" {
				select {
				case <-port.started:
				case <-time.After(time.Second):
					t.Fatal("handshake did not reach the blocked read")
				}
				cancel()
			}
			select {
			case err := <-completed:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("handshake error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("serial handshake ignored cancellation")
			}
			// Failed-connect cleanup and a later client Close must share the
			// cancellation callback's guarded teardown.
			if err := link.close(); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			port.mu.Lock()
			defer port.mu.Unlock()
			if port.lateCalls != 0 {
				t.Fatalf("%d driver calls occurred after the port closed", port.lateCalls)
			}
			if got := port.events[len(port.events)-3:]; got[0] != "exit" || got[1] != "drain" || got[2] != "close" {
				t.Fatalf("teardown events = %v", port.events)
			}
		})
	}
}
