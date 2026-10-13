package bleprovider

import (
	"context"
	"errors"
	"net"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// StreamNode is implemented by localmesh.Node.AttachStream. The provider
// supplies a mutually authenticated TLS 1.3 byte stream and the expensive BLE
// Babel cost. It never wraps BLE in QUIC.
type StreamNode interface {
	AttachStream(context.Context, int32, net.Conn, uint16) error
}

type Config struct {
	Credentials *localmesh.Credentials
	Node        StreamNode
	MeshName    string
	PSM         uint16                   // zero selects DefaultPSM
	TargetPeers int                      // zero selects DefaultTargetPeers
	AdapterPath string                   // empty selects the first advertising-capable BlueZ adapter
	Selection   *localmesh.PeerSelection // shared authenticated LAN and radio diversity policy
	Logger      *zap.Logger
}

func (c *Config) defaults() error {
	if c.Credentials == nil || c.Node == nil || c.MeshName == "" {
		return errors.New("BLE requires Wendy credentials, a stream node, and a mesh name")
	}
	if c.PSM == 0 {
		c.PSM = DefaultPSM
	}
	if !ValidPSM(c.PSM) {
		return errors.New("BLE PSM must be an LE dynamic PSM (0x0080–0x00ff)")
	}
	if c.TargetPeers == 0 {
		c.TargetPeers = DefaultTargetPeers
	}
	if c.TargetPeers < 1 || c.TargetPeers > 16 {
		return errors.New("BLE target peers must be 1–16")
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	return nil
}
