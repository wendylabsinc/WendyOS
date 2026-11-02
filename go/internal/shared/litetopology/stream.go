package litetopology

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ReadStream uses a private authenticated WendyCom stream. Cancellation closes
// it, including an in-progress frame write; callers own the connection lifetime.
func ReadStream(ctx context.Context, conn net.Conn) (*pb.LocalMeshTopology, error) {
	handshake, cancel := context.WithTimeout(ctx, HandshakeBudget)
	read, err := OpenStream(handshake, conn)
	cancel()
	if err != nil {
		return nil, err
	}
	return Read(ctx, read)
}

// OpenStream performs only the WendyCom handshake on a private authenticated
// stream. TLS and WendyCom can share one setup deadline; later pages receive
// independent short deadlines and a separate aggregate budget.
func OpenStream(ctx context.Context, conn net.Conn) (ReadPage, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint32(random[:])
	wire := &stream{conn: conn, limitDiscards: true, discardRemaining: streamDiscardBytes}
	if err := wire.send(&lite.WendyComMessage{Msg: &lite.WendyComMessage_Handshake{Handshake: &lite.WendyComHandshake{HandshakeId: id, Version: &lite.WendyComProtocolVersion{Major: 2}}}}); err != nil {
		return nil, err
	}
	matched := false
	for skipped := 0; skipped < streamReplyFrames; skipped++ {
		reply, err := wire.receive()
		if err != nil {
			return nil, err
		}
		switch reply.GetMsg().(type) {
		case nil, *lite.WendyComMessage_Event:
			// Unknown oneofs and unsolicited events carry no admission result.
			if err := wire.discardFrame(); err != nil {
				return nil, err
			}
			continue
		case *lite.WendyComMessage_Handshake:
		default:
			return nil, status.Error(codes.Unimplemented, "unexpected WendyCom message before handshake")
		}
		handshake := reply.GetHandshake()
		if handshake.HandshakeId != id || handshake.GetVersion().GetMajor() != 2 {
			return nil, status.Error(codes.Unimplemented, "unsupported WendyCom protocol")
		}
		matched = true
		break
	}
	if !matched {
		return nil, status.Error(codes.Unavailable, "no matching WendyCom handshake")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return func(parent context.Context, params *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		ctx, cancel := context.WithTimeout(parent, PageBudget)
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		id++
		wire.discardRemaining = streamDiscardBytes
		if err := wire.send(&lite.WendyComMessage{Msg: &lite.WendyComMessage_Command{Command: &lite.WendyComCommand{RequestId: id, Params: &lite.WendyComCommand_NetTopology{NetTopology: params}}}}); err != nil {
			return nil, err
		}
		for skipped := 0; skipped < streamReplyFrames; skipped++ {
			reply, err := wire.receive()
			if err != nil {
				return nil, err
			}
			response := reply.GetResponse()
			if response == nil || response.RequestId != id {
				if err := wire.discardFrame(); err != nil {
					return nil, err
				}
				continue
			}
			if response.Result == lite.WendyComResult_WENDY_COM_RESULT_BUSY {
				return nil, status.Error(codes.Aborted, "topology view changed or is busy")
			}
			if response.Result != lite.WendyComResult_WENDY_COM_RESULT_OK {
				return nil, status.Error(codes.Unimplemented, "topology is unsupported or not ready")
			}
			return response.GetNetTopology(), nil
		}
		return nil, status.Error(codes.Unavailable, "no matching topology reply")
	}, nil
}

const (
	streamReplyFrames = 32
	// Only discarded extensions/unmatched messages consume this budget.
	// Matching topology pages retain collect's independent 512 KiB limit.
	streamDiscardBytes = 128 << 10
)

type stream struct {
	conn             net.Conn
	limitDiscards    bool
	discardRemaining int
	frameBytes       int
	frameDiscarded   bool
}

func (s *stream) discardFrame() error {
	if s.frameDiscarded {
		return nil
	}
	if s.limitDiscards {
		if s.frameBytes > s.discardRemaining {
			return status.Error(codes.ResourceExhausted, "WendyCom extensions exceed discard byte budget")
		}
		s.discardRemaining -= s.frameBytes
	}
	s.frameDiscarded = true
	return nil
}

func (s *stream) send(message *lite.WendyComMessage) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	header := []byte{0xa5, 2, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(header[6:], uint16(len(body)))
	for _, part := range [][]byte{header, body} {
		for len(part) > 0 {
			n, err := s.conn.Write(part)
			if err != nil {
				return err
			}
			if n <= 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}
func (s *stream) receive() (*lite.WendyComMessage, error) {
	var header [8]byte
	if _, err := io.ReadFull(s.conn, header[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(header[6:]))
	if header[0] != 0xa5 || header[1] != 2 {
		return nil, fmt.Errorf("invalid WendyCom topology frame")
	}
	s.frameBytes, s.frameDiscarded = len(header)+size, false
	if header[2] != 0 || header[3] != 0 {
		// The u16 body length delimits a future category/channel even when
		// its bytes are not protobuf. Keep the original phase deadline.
		if err := s.discardFrame(); err != nil {
			return nil, err
		}
		if _, err := io.CopyN(io.Discard, s.conn, int64(size)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if size == 0 || size > 4096 {
		return nil, fmt.Errorf("invalid WendyCom topology frame")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(s.conn, body); err != nil {
		return nil, err
	}
	message := new(lite.WendyComMessage)
	if err := proto.Unmarshal(body, message); err != nil {
		return nil, err
	}
	return message, nil
}
