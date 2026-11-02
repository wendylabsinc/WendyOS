// Package meshappstream opens an authorized application TCP stream on an
// existing authenticated agent connection. It adds no inner TLS connection.
package meshappstream

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

const ChunkSize = 16 * 1024

// Dial waits for the destination's running-app port admission before returning.
// The caller owns cc and its original operator/device authentication. Each flow
// owns only its gRPC stream; closing it leaves other RPCs on cc running.
func Dial(ctx context.Context, cc *grpc.ClientConn, port uint16) (net.Conn, error) {
	if cc == nil || port == 0 {
		return nil, errors.New("application stream requires an authenticated agent connection and port")
	}
	owner, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	fail := func(err error) (net.Conn, error) { stop(); cancel(); return nil, err }
	stream, err := pb.NewWendyMeshServiceClient(cc).MeshDial(owner, grpc.MaxCallSendMsgSize(ChunkSize+64), grpc.MaxCallRecvMsgSize(ChunkSize+64))
	if err != nil {
		return fail(err)
	}
	if err = stream.Send(&pb.MeshDialMessage{Content: &pb.MeshDialMessage_Open{Open: &pb.MeshDialOpen{Port: uint32(port)}}}); err != nil {
		return fail(err)
	}
	ready, err := stream.Recv()
	if err != nil {
		return fail(err)
	}
	if len(ready.Payload) != 0 || ready.HalfClose {
		return fail(errors.New("mesh application peer did not acknowledge port admission"))
	}
	if !stop() && ctx.Err() != nil {
		return fail(ctx.Err())
	}
	// Independent pipes preserve half-close in either direction and provide
	// actual per-direction deadlines without an unbounded receive queue.
	read, incoming := net.Pipe()
	write, outgoing := net.Pipe()
	conn := &Conn{read: read, incoming: incoming, write: write, outgoing: outgoing, cancel: cancel, writeDone: make(chan struct{}), closed: make(chan struct{}), writeDeadlineChanged: make(chan struct{}, 1)}
	context.AfterFunc(owner, func() { _ = conn.Close() })
	go conn.receive(stream)
	go conn.send(stream)
	return conn, nil
}

type Conn struct {
	read, incoming, write, outgoing net.Conn
	cancel                          context.CancelFunc
	once                            sync.Once
	writeMu                         sync.Mutex
	writeClosed                     atomic.Bool
	writeDone                       chan struct{}
	writeErr                        error // Published before writeDone closes.
	closed                          chan struct{}
	deadlineMu                      sync.Mutex
	writeDeadline                   time.Time
	writeDeadlineChanged            chan struct{}
	readMu                          sync.Mutex
	readErr                         error
}

func (c *Conn) receive(stream pb.WendyMeshService_MeshDialClient) {
	defer c.incoming.Close()
	for {
		message, err := stream.Recv()
		if err != nil {
			c.readMu.Lock()
			c.readErr = err
			c.readMu.Unlock()
			if !errors.Is(err, io.EOF) {
				_ = c.Close()
			}
			return
		}
		if len(message.Payload) > ChunkSize {
			_ = c.Close()
			return
		}
		if len(message.Payload) > 0 {
			if _, err := c.incoming.Write(message.Payload); err != nil {
				return
			}
		}
		if message.HalfClose {
			return
		}
	}
}

func (c *Conn) send(stream pb.WendyMeshService_MeshDialClient) {
	defer close(c.writeDone)
	defer func() {
		if err := stream.CloseSend(); c.writeErr == nil {
			c.writeErr = err
		}
	}()
	buffer := make([]byte, ChunkSize)
	for {
		n, err := c.outgoing.Read(buffer)
		if n > 0 {
			if err := stream.Send(&pb.MeshDialMessage{Content: &pb.MeshDialMessage_Data{Data: &pb.MeshDialData{Payload: append([]byte(nil), buffer[:n]...)}}}); err != nil {
				c.writeErr = err
				_ = c.Close()
				return
			}
		}
		if err != nil {
			if c.writeClosed.Load() {
				c.writeErr = stream.Send(&pb.MeshDialMessage{Content: &pb.MeshDialMessage_Data{Data: &pb.MeshDialData{HalfClose: true}}})
				if c.writeErr != nil {
					_ = c.Close()
				}
			} else {
				c.writeErr = err
				_ = c.Close()
			}
			return
		}
	}
}

func (c *Conn) Read(buffer []byte) (int, error) {
	n, err := c.read.Read(buffer)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		c.readMu.Lock()
		if c.readErr != nil {
			err = c.readErr
		}
		c.readMu.Unlock()
	}
	return n, err
}
func (c *Conn) Write(buffer []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed.Load() {
		return 0, net.ErrClosed
	}
	return c.write.Write(buffer)
}
func (c *Conn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.writeClosed.Swap(true) {
		_ = c.write.Close()
	}
	for {
		select {
		case <-c.writeDone:
			return c.writeErr
		default:
		}
		c.deadlineMu.Lock()
		deadline := c.writeDeadline
		c.deadlineMu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				// A buffered chunk may already be inside a blocked gRPC Send.
				// Cancel only this flow so its sender and receiver can retire.
				_ = c.Close()
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case <-c.writeDone:
			if timer != nil {
				timer.Stop()
			}
			return c.writeErr
		case <-c.closed:
			if timer != nil {
				timer.Stop()
			}
			return net.ErrClosed
		case <-c.writeDeadlineChanged:
		case <-expired:
		}
		if timer != nil {
			timer.Stop()
		}
		// Re-sample after either event: another caller may have extended or
		// cleared the deadline while this iteration's timer was firing.
	}
}
func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.cancel()
		_ = c.read.Close()
		_ = c.incoming.Close()
		_ = c.write.Close()
		_ = c.outgoing.Close()
	})
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error { return c.read.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error {
	// The local writer closes before its buffered final frame drains. Keep
	// deadline updates usable during that half-close, including shortening or
	// clearing an existing wait; the whole connection still closes normally.
	if err := c.write.SetWriteDeadline(t); err != nil && !c.writeClosed.Load() {
		return err
	}
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	c.deadlineMu.Lock()
	c.writeDeadline = t
	c.deadlineMu.Unlock()
	select {
	case c.writeDeadlineChanged <- struct{}{}:
	default:
	}
	return nil
}
func (c *Conn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
func (c *Conn) LocalAddr() net.Addr  { return streamAddr{} }
func (c *Conn) RemoteAddr() net.Addr { return streamAddr{} }

type streamAddr struct{}

func (streamAddr) Network() string { return "wendy-net-app" }
func (streamAddr) String() string  { return "wendy-net-app" }
