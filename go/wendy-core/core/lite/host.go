package lite

import (
	"context"
	"fmt"
	"time"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

type hostLink struct{ platform.LiteLink }

func (l hostLink) linkHandshake() error { return l.Handshake() }
func (l hostLink) send(m *pb.WendyComMessage) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	return l.Send(data)
}
func (l hostLink) recv(timeout time.Duration) (*pb.WendyComMessage, error) {
	data, err := l.Receive(timeout)
	if err != nil {
		return nil, err
	}
	m := new(pb.WendyComMessage)
	err = proto.Unmarshal(data, m)
	return m, err
}
func (l hostLink) preferredChunkSize() int { return l.PreferredChunkSize() }
func (l hostLink) close() error            { return l.Close() }

// Connect negotiates WendyCom and starts dispatching messages. The caller owns
// connection setup; a failed negotiation closes the supplied link. A Client is
// single-use after successful connection. Closing the link cancels negotiation.
func (c *Client) Connect(ctx context.Context, link platform.LiteLink) error {
	if link == nil || c.entropy == nil {
		if link != nil {
			link.Close()
		}
		return fmt.Errorf("host Lite link and entropy are required")
	}
	if err := ctx.Err(); err != nil {
		link.Close()
		return err
	}
	if c.link != nil {
		link.Close()
		return fmt.Errorf("Lite client is already connected")
	}
	if size := link.PreferredChunkSize(); size <= 0 || size > 65535 {
		link.Close()
		return fmt.Errorf("invalid host Lite chunk size %d", size)
	}
	c.link = hostLink{link}
	stop := context.AfterFunc(ctx, func() { link.Close() })
	defer stop()
	if err := c.handshakeContext(ctx); err != nil {
		link.Close()
		c.link = nil
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("handshake: %w", err)
	}
	if err := ctx.Err(); err != nil {
		link.Close()
		c.link = nil
		return err
	}
	c.startReadLoop()
	return nil
}
