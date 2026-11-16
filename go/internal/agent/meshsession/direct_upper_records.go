package meshsession

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

// DirectUpperRecords carries retained-session control on an already authenticated
// application stream. Each record has a uint16 big-endian byte length followed by
// that many bytes, bounded by MaxFlowFrame. Application payload uses separate raw
// promoted streams, never this lane. Framing supplies no authentication itself.
//
// The adapter owns conn. Both queues have one slot; TrySend copies before return
// and never waits for transport I/O. Close aborts the connection and joins all
// workers. Underlay loss must not cancel parent while the upper transport lives.
type DirectUpperRecords struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	send    chan upperControlRecord
	receive chan []byte
	done    chan struct{}
	pending atomic.Int64 // queued plus writer-owned records, including partial writes
}
type upperControlRecord struct {
	size int
	wire [2 + directstream.MaxFlowFrame]byte
}

func NewDirectUpperRecords(parent context.Context, conn net.Conn) (*DirectUpperRecords, error) {
	if conn == nil {
		return nil, directstream.ErrBounds
	}
	ctx, cancel := context.WithCancelCause(parent)
	r := &DirectUpperRecords{ctx: ctx, cancel: cancel, send: make(chan upperControlRecord, 1), receive: make(chan []byte, 1), done: make(chan struct{})}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for {
			var head [2]byte
			if _, err := io.ReadFull(conn, head[:]); err != nil {
				cancel(err)
				return
			}
			n := int(binary.BigEndian.Uint16(head[:]))
			if n == 0 || n > directstream.MaxFlowFrame {
				cancel(directstream.ErrBounds)
				return
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(conn, data); err != nil {
				cancel(err)
				return
			}
			select {
			case r.receive <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case frame := <-r.send:
				data := frame.wire[:frame.size]
				for len(data) != 0 {
					n, err := conn.Write(data)
					if err != nil {
						cancel(err)
						return
					}
					if n == 0 {
						cancel(io.ErrShortWrite)
						return
					}
					data = data[n:]
				}
				r.pending.Add(-1)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		conn.Close()
		workers.Wait()
		close(r.done)
	}()
	return r, nil
}

func (r *DirectUpperRecords) Close() error {
	r.cancel(net.ErrClosed)
	<-r.done
	return nil
}
func (r *DirectUpperRecords) TrySendDirectRecord(data []byte) (bool, error) {
	if len(data) == 0 || len(data) > directstream.MaxFlowFrame {
		return false, directstream.ErrBounds
	}
	if err := context.Cause(r.ctx); err != nil {
		return false, err
	}
	var frame upperControlRecord
	frame.size = 2 + len(data)
	binary.BigEndian.PutUint16(frame.wire[:2], uint16(len(data)))
	copy(frame.wire[2:], data)
	r.pending.Add(1)
	select {
	case <-r.ctx.Done():
		r.pending.Add(-1)
		return false, context.Cause(r.ctx)
	case r.send <- frame:
		return true, nil
	default:
		r.pending.Add(-1)
		return false, nil
	}
}

// DirectRecordsDrained includes a record removed from the queue but still in a
// partial transport Write. The session actor stops producing records before
// using this as the final orderly-retirement fence.
func (r *DirectUpperRecords) DirectRecordsDrained() bool {
	return r.pending.Load() == 0
}
func (r *DirectUpperRecords) ReceiveDirectRecord(ctx context.Context) ([]byte, error) {
	select {
	case data := <-r.receive:
		return data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		// EOF follows every complete record already read. Preserve that order
		// even when the reader observes EOF before the actor drains its queue.
		select {
		case data := <-r.receive:
			return data, nil
		default:
			return nil, context.Cause(r.ctx)
		}
	}
}
