package bleprovider

import (
	"context"
	"errors"
	"net"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// StreamNode is implemented by localmesh.Node.AttachStream. The provider
// supplies a mutually authenticated TLS 1.3 byte stream and the expensive BLE
// Babel cost. It never wraps BLE in QUIC.
type StreamNode interface {
	AttachStream(context.Context, meshid.Device, net.Conn, uint16) error
}

type Config struct {
	Credentials *localmesh.Credentials
	Node        StreamNode
	NetworkName string
	PSM         uint16                   // zero selects DefaultPSM
	AdapterPath string                   // empty selects the first advertising-capable BlueZ adapter
	Selection   *localmesh.PeerSelection // shared authenticated LAN and radio diversity policy
	// Policy may be shared with other consumers of the same physical adapter.
	// A nil policy creates the standard router BLE limits.
	Policy     *localmesh.ConnectionPolicy
	Pacer      *DialPacer                // optional shared physical-adapter outbound start spacing
	Candidates *localmesh.CandidateQueue // optional shared physical-adapter failure history
	Logger     *zap.Logger
	// Status receives local setup readiness and discovery failures/recovery,
	// not peer reachability. Discovery failures preserve established links.
	// The optional callback must return promptly.
	Status func(localmesh.CarrierStatus)
}

func (c *Config) defaults() error {
	if c.Credentials == nil || c.Node == nil || c.NetworkName == "" {
		return errors.New("BLE requires Wendy credentials, a stream node, and a wendy net name")
	}
	if c.PSM == 0 {
		c.PSM = DefaultPSM
	}
	if !ValidPSM(c.PSM) {
		return errors.New("BLE PSM must be an LE dynamic PSM (0x0080–0x00ff)")
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	return nil
}
