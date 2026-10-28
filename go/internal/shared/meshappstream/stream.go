// Package meshappstream opens an authorized application TCP stream on an
// existing authenticated agent connection. It adds no inner TLS connection.
package meshappstream

import (
	"context"
	"errors"
	"io"
	"net"
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
	conn := &Conn{read: read, incoming: incoming, write: write, outgoing: outgoing, cancel: cancel, writeDone: make(chan struct{})}
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
	defer stream.CloseSend()
	buffer := make([]byte, ChunkSize)
	for {
		n, err := c.outgoing.Read(buffer)
		if n > 0 {
			if err := stream.Send(&pb.MeshDialMessage{Content: &pb.MeshDialMessage_Data{Data: &pb.MeshDialData{Payload: append([]byte(nil), buffer[:n]...)}}}); err != nil {
				_ = c.Close()
				return
			}
		}
		if err != nil {
			if c.writeClosed.Load() {
				_ = stream.Send(&pb.MeshDialMessage{Content: &pb.MeshDialMessage_Data{Data: &pb.MeshDialData{HalfClose: true}}})
			} else {
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
	<-c.writeDone
	return nil
}
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.cancel()
		_ = c.read.Close()
		_ = c.incoming.Close()
		_ = c.write.Close()
		_ = c.outgoing.Close()
	})
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.read.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.write.SetWriteDeadline(t) }
func (c *Conn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
func (c *Conn) LocalAddr() net.Addr  { return streamAddr{} }
func (c *Conn) RemoteAddr() net.Addr { return streamAddr{} }

type streamAddr struct{}

func (streamAddr) Network() string { return "wendy-net-app" }
func (streamAddr) String() string  { return "wendy-net-app" }
