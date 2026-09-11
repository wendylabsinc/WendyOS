package mcusource

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const grpcDropLogInterval = 5 * time.Second

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
	tlsCfg, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, logger, strconv.Itoa(int(p.SourceAssetID)))
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
	return &grpcTransport{
		logger: logger.With(zap.Int32("source", p.SourceAssetID), zap.String("addr", addr)),
		cc:     cc,
		client: agentpbv2.NewWendySensorServiceClient(cc),
	}, nil
}

// NewInsecureGRPCTransportForTest dials without TLS — for in-process tests only.
func NewInsecureGRPCTransportForTest(addr string) (SensorTransport, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcTransport{logger: zap.NewNop(), cc: cc, client: agentpbv2.NewWendySensorServiceClient(cc)}, nil
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
	logger := t.logger.With(zap.Uint32s("channels", append([]uint32(nil), channels...)))
	go func() {
		defer close(frames)
		defer cancel()
		var dropped, droppedTotal uint64
		var lastDropLog time.Time
		logDrops := func() {
			if dropped == 0 {
				return
			}
			logger.Warn("sensor grpc stream dropped frames under backpressure",
				zap.Uint64("dropped", dropped),
				zap.Uint64("dropped_total", droppedTotal))
			dropped = 0
			lastDropLog = time.Now()
		}
		// Flush even a short burst when the stream ends or is canceled.
		defer logDrops()
		for {
			f, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-sctx.Done():
				return
			default:
				// Keep the bounded, nonblocking queue so a slow consumer
				// does not stall the source or accumulate stale frames.
				dropped++
				droppedTotal++
			}
			// Warn immediately on the first drop, then aggregate at most
			// once per interval. Successful receives also flush pending
			// counts after recovery; an idle stream flushes on exit.
			if dropped > 0 && (lastDropLog.IsZero() || time.Since(lastDropLog) >= grpcDropLogInterval) {
				logDrops()
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
