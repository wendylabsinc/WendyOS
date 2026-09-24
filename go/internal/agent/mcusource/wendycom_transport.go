package mcusource

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

const (
	// wendycomManifestTimeout applies when FetchManifest's ctx has no deadline.
	wendycomManifestTimeout = 3 * time.Second
	// wendycomSubscribeTimeout bounds the Subscribe round trip; the stream
	// context carries no deadline of its own.
	wendycomSubscribeTimeout = 3 * time.Second
	// wendycomUnsubscribeTimeout bounds the best-effort Unsubscribe on
	// teardown, which Runner.Stop ends up waiting for.
	wendycomUnsubscribeTimeout = time.Second
)

// wendycomClient is the part of liteclient.WendyLiteClient the transport
// uses, so tests can drive the transport without a device.
type wendycomClient interface {
	GetSensorManifest(timeout time.Duration) (*sensorlinkpb.SensorManifest, error)
	SensorLinkSubscribe(channelIDs []uint32, timeout time.Duration) error
	SensorLinkUnsubscribe(channelIDs []uint32, timeout time.Duration) error
	AddSensorFrameListener(fn func(*sensorlinkpb.SensorFrame)) func()
	Done() <-chan struct{}
	Close() error
}

// wendycomTransport implements SensorTransport over WendyCom, the protocol
// Wendy Lite boards speak. Unlike tcpTransport it keeps one connection for
// both the manifest and the stream: a WendyCom handshake is costly on an
// ESP32.
type wendycomTransport struct {
	logger  *zap.Logger
	connect func() (wendycomClient, error)

	mu     sync.Mutex
	client wendycomClient // nil until the first successful connect
	closed bool

	closeOnce sync.Once
	closeErr  error
}

// NewWendyComTransport reaches a Wendy Lite board over LAN at addr.
//
// TODO: this connects with ConnectInsecure — no client certificate, and no
// check of the board's certificate — so the manifest's device_asset_id is the
// only identity check. Move to mTLS pinned to p.SourceAssetID, as mtlsDialer
// does for the other transports.
func NewWendyComTransport(logger *zap.Logger, p SensorPairing, addr string) SensorTransport {
	return &wendycomTransport{
		logger: logger.With(zap.Int32("source", p.SourceAssetID), zap.String("addr", addr)),
		connect: func() (wendycomClient, error) {
			c := liteclient.NewWendyLiteClient()
			if err := c.ConnectInsecure(addr); err != nil {
				return nil, fmt.Errorf("mcusource: wendycom connect %s: %w", addr, err)
			}
			return c, nil
		},
	}
}

// clientFor returns the transport's connection, opening it on first use.
// ConnectInsecure takes no context and its TCP dial has no timeout, so ctx
// bounds the wait here instead: a dead address would otherwise hold up
// Runner.Stop for minutes. A connect that finishes after ctx gave up is closed
// rather than leaked.
func (t *wendycomTransport) clientFor(ctx context.Context) (wendycomClient, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("mcusource: wendycom transport closed")
	}
	if t.client != nil {
		return t.client, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		c   wendycomClient
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := t.connect()
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		t.client = r.c
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-done; r.err == nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("mcusource: wendycom connect: %w", ctx.Err())
	}
}

func (t *wendycomTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	c, err := t.clientFor(ctx)
	if err != nil {
		return nil, err
	}
	// The client takes a timeout, not a context, and a timeout <= 0 would
	// mean no deadline at all.
	timeout := wendycomManifestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if timeout = time.Until(deadline); timeout <= 0 {
			return nil, fmt.Errorf("mcusource: wendycom manifest: %w", context.DeadlineExceeded)
		}
	}
	m, err := c.GetSensorManifest(timeout)
	if err != nil {
		return nil, fmt.Errorf("mcusource: wendycom manifest: %w", err)
	}
	return m, nil
}

func (t *wendycomTransport) Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	c, err := t.clientFor(ctx)
	if err != nil {
		return nil, nil, err
	}
	s := &wendycomStream{
		logger: t.logger.With(zap.Uint32s("channels", append([]uint32(nil), channels...))),
		frames: make(chan *sensorlinkpb.SensorFrame, 8),
	}
	// Listen before subscribing so the first frames are not lost.
	removeListener := c.AddSensorFrameListener(s.deliver)
	if err := c.SensorLinkSubscribe(channels, wendycomSubscribeTimeout); err != nil {
		removeListener()
		return nil, nil, fmt.Errorf("mcusource: wendycom subscribe: %w", err)
	}
	stop := make(chan struct{})
	go func() {
		// Frames carry no end-of-stream marker, so the stream ends when the
		// board drops off, when ctx is cancelled, or when closeFn runs.
		select {
		case <-c.Done():
		case <-ctx.Done():
		case <-stop:
		}
		s.finish()
	}()
	var once sync.Once
	closeFn := func() error {
		once.Do(func() {
			removeListener()
			s.finish()
			close(stop)
			select {
			case <-c.Done():
			default:
				// Best effort: Close follows, and the board drops its
				// subscriptions when the connection goes, but this stops the
				// capture right away.
				if err := c.SensorLinkUnsubscribe(channels, wendycomUnsubscribeTimeout); err != nil {
					s.logger.Debug("sensor wendycom unsubscribe failed", zap.Error(err))
				}
			}
		})
		return nil
	}
	return s.frames, closeFn, nil
}

func (t *wendycomTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.closed = true
		if t.client != nil {
			t.closeErr = t.client.Close()
		}
	})
	return t.closeErr
}

// wendycomStream feeds one Stream's frames channel from the client's read
// loop.
type wendycomStream struct {
	logger *zap.Logger
	frames chan *sensorlinkpb.SensorFrame

	// mu orders deliver against finish. A deliver can still be running after
	// its listener is removed — the client dispatches to a snapshot of its
	// listeners without a lock — and it must never send on a closed channel.
	mu                    sync.Mutex
	closed                bool
	dropped, droppedTotal uint64
	lastDropLog           time.Time
}

// deliver runs on the client's read loop, so it must never block: a full
// queue drops the frame rather than stalling the whole connection, as the
// other transports do.
func (s *wendycomStream) deliver(f *sensorlinkpb.SensorFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.frames <- f:
	default:
		s.dropped++
		s.droppedTotal++
	}
	// Warn immediately on the first drop, then aggregate at most once per
	// interval, as grpcTransport does.
	if s.dropped > 0 && (s.lastDropLog.IsZero() || time.Since(s.lastDropLog) >= grpcDropLogInterval) {
		s.logDropsLocked()
	}
}

// finish closes frames once, flushing any drop count not yet logged.
func (s *wendycomStream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.frames)
	s.logDropsLocked()
}

func (s *wendycomStream) logDropsLocked() {
	if s.dropped == 0 {
		return
	}
	s.logger.Warn("sensor wendycom stream dropped frames under backpressure",
		zap.Uint64("dropped", s.dropped),
		zap.Uint64("dropped_total", s.droppedTotal))
	s.dropped = 0
	s.lastDropLog = time.Now()
}
