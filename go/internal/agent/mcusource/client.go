package mcusource

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// Dialer opens a transport connection to a sensor source. The production
// implementation returns an mTLS net.Conn (see mtlsDialer); tests use plain TCP.
type Dialer interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

// Stream is an active sensorlink session. Frames is closed when the session ends.
type Stream struct {
	Manifest  *sensorlinkpb.SensorManifest
	Frames    <-chan *sensorlinkpb.SensorFrame
	conn      net.Conn
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

// Connect dials the source, reads its manifest, subscribes to channels, and
// streams frames until Close or a read error.
func Connect(ctx context.Context, d Dialer, addr string, channels []uint32) (*Stream, error) {
	conn, err := d.Dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("mcusource: dial %s: %w", addr, err)
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &Stream{conn: conn, cancel: cancel}
	// Cancellation must also interrupt the manifest/subscription handshake.
	context.AfterFunc(sctx, func() { _ = s.Close() })
	env, err := sensorlink.ReadMessage(conn)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("mcusource: read manifest: %w", err)
	}
	manifest := env.GetManifest()
	if manifest == nil {
		s.Close()
		return nil, fmt.Errorf("mcusource: first message was not a manifest")
	}
	if err := sensorlink.WriteMessage(conn, &sensorlinkpb.Envelope{Msg: &sensorlinkpb.Envelope_Subscribe{Subscribe: &sensorlinkpb.Subscribe{ChannelId: channels}}}); err != nil {
		s.Close()
		return nil, fmt.Errorf("mcusource: subscribe: %w", err)
	}
	frames := make(chan *sensorlinkpb.SensorFrame, 8)
	s.Manifest, s.Frames = manifest, frames
	go func() {
		defer close(frames)
		defer s.Close()
		for {
			env, err := sensorlink.ReadMessage(conn)
			if err != nil {
				return
			}
			if f := env.GetFrame(); f != nil {
				select {
				case frames <- f:
				case <-sctx.Done():
					return
				default:
					// Backpressure: drop rather than block the source.
				}
			}
		}
	}()
	return s, nil
}
