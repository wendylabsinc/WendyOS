package services

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

// CloudForwarder subscribes to the TelemetryBroadcaster and forwards log batches
// to the configured cloud gRPC endpoint. It reconnects automatically on failure.
//
// For the POC this uses a plaintext connection with no auth. Before production:
// - swap insecure.NewCredentials() for the device mTLS cert
// - add retry with exponential backoff
// - add a disk-backed queue so logs survive network outages
type CloudForwarder struct {
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	cloudGRPC   string
}

func NewCloudForwarder(logger *zap.Logger, broadcaster *TelemetryBroadcaster, cloudGRPC string) *CloudForwarder {
	return &CloudForwarder{
		logger:      logger,
		broadcaster: broadcaster,
		cloudGRPC:   cloudGRPC,
	}
}

// Run loops forever, connecting to the cloud and forwarding logs. On any failure
// it waits briefly and retries. It exits cleanly when ctx is cancelled.
func (f *CloudForwarder) Run(ctx context.Context) {
	f.logger.Info("cloud log forwarder starting", zap.String("endpoint", f.cloudGRPC))
	for {
		if err := f.runOnce(ctx); err != nil {
			f.logger.Warn("cloud log forwarder disconnected", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			f.logger.Info("cloud log forwarder stopped")
			return
		case <-time.After(5 * time.Second):
			f.logger.Info("cloud log forwarder reconnecting")
		}
	}
}

// runOnce opens a connection to the cloud, subscribes to logs, and forwards
// each batch until the connection fails or ctx is cancelled.
func (f *CloudForwarder) runOnce(ctx context.Context) error {
	conn, err := grpc.NewClient(f.cloudGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := otelpb.NewLogsServiceClient(conn)

	subID, ch := f.broadcaster.SubscribeLogs()
	defer f.broadcaster.UnsubscribeLogs(subID)

	f.logger.Info("cloud log forwarder connected", zap.String("endpoint", f.cloudGRPC))

	for {
		select {
		case <-ctx.Done():
			return nil
		case batch, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := client.Export(ctx, batch); err != nil {
				return err
			}
		}
	}
}
