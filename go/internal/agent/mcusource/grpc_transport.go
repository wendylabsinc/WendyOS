package mcusource

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcTransport implements SensorTransport by calling the agent's own
// WendySensorService (the gRPC path for agent-hosted sensor sources, as
// opposed to tcpTransport's raw-TCP path for MCUs).
type grpcTransport struct {
	closeOnce sync.Once
	closeErr  error
	logger    *zap.Logger
	cc        *grpc.ClientConn
	client    agentpbv2.WendySensorServiceClient
}

// NewGRPCTransport dials the source's mTLS agent endpoint, pinning its identity.
func NewGRPCTransport(logger *zap.Logger, certPEM, chainPEM, keyPEM string, p SensorPairing, addr string) (SensorTransport, error) {
	tlsCfg, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, logger, p.OrgID, strconv.Itoa(int(p.SourceAssetID)))
	if err != nil {
		return nil, fmt.Errorf("mcusource: grpc tls: %w", err)
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	cc, err := grpc.NewClient("passthrough:///sensor-source",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("mcusource: grpc dial %s: %w", addr, err)
	}
	return &grpcTransport{logger: logger, cc: cc, client: agentpbv2.NewWendySensorServiceClient(cc)}, nil
}

// NewInsecureGRPCTransportForTest dials without TLS — for in-process tests only.
func NewInsecureGRPCTransportForTest(addr string) (SensorTransport, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcTransport{cc: cc, client: agentpbv2.NewWendySensorServiceClient(cc)}, nil
}

func (t *grpcTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	return t.client.GetSensorManifest(ctx, &agentpbv2.GetSensorManifestRequest{})
}

func (t *grpcTransport) Stream(ctx context.Context, channels []uint32) (<-chan *sensorlinkpb.SensorFrame, func() error, error) {
	sctx, cancel := context.WithCancel(ctx)
	stream, err := t.client.StreamSensors(sctx, &agentpbv2.StreamSensorsRequest{ChannelId: channels})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("mcusource: StreamSensors: %w", err)
	}
	frames := make(chan *sensorlinkpb.SensorFrame, 8)
	go func() {
		defer close(frames)
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-sctx.Done():
				return
			default: // backpressure: drop rather than block the source
			}
		}
	}()
	closeFn := func() error { cancel(); return nil }
	return frames, closeFn, nil
}

func (t *grpcTransport) Close() error {
	t.closeOnce.Do(func() { t.closeErr = t.cc.Close() })
	return t.closeErr
}
