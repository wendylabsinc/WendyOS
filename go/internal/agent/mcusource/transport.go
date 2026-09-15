package mcusource

import (
	"context"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// SensorTransport abstracts how a source's manifest and frames are obtained,
// so the supervisor is transport-agnostic (raw-TCP for MCUs, gRPC for agents).
type SensorTransport interface {
	// Close releases transport resources, even if no stream was opened.
	Close() error
	FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error)
	Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error)
}

// TransportFactory builds a transport for a pairing at a resolved address.
type TransportFactory func(p SensorPairing, addr string) (SensorTransport, error)

// tcpTransport wraps the Plan 1 raw-TCP Connect (the MCU path).
type tcpTransport struct {
	d    Dialer
	addr string
}

func NewTCPTransport(d Dialer, addr string) SensorTransport { return &tcpTransport{d: d, addr: addr} }

func (t *tcpTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	s, err := Connect(ctx, t.d, t.addr, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Manifest, nil
}

func (t *tcpTransport) Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	s, err := Connect(ctx, t.d, t.addr, channels)
	if err != nil {
		return nil, nil, err
	}
	return s.Frames, s.Close, nil
}

// TCP sessions own their connections; FetchManifest and the stream closer release them.
func (t *tcpTransport) Close() error { return nil }
