package services

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// normalizeCloudHost ensures host has a port component, defaulting to :443.
func normalizeCloudHost(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "443")
}

const (
	cloudFlusherMaxBackoff = 60 * time.Second
	// cloudFlusherFramesPerPass bounds how many buffered OTLP frames a single
	// flush pass reads (and exports, one Export RPC each) before looping.
	cloudFlusherFramesPerPass = 500
)

// CloudFlusher reads buffered OTLP frames from TelemetryBuffer via ReadFromCursor
// and re-exports them to the cloud's standard OTLP collector routes
// (Logs/Metrics/Trace Service Export) over mTLS gRPC. PII/size guards are
// applied per frame before export. Identity is derived server-side from the
// client certificate.
type CloudFlusher struct {
	logger          *zap.Logger
	buffer          *TelemetryBuffer
	provisioningSvc *ProvisioningService // nil in tests
}

// NewCloudFlusher creates a CloudFlusher for tests. The explicit org/asset ID
// parameters are preserved for compatibility with existing callers, but the
// flusher does not store them on the struct. They are ignored entirely because
// device identity is derived from the mTLS client certificate.
func NewCloudFlusher(logger *zap.Logger, buffer *TelemetryBuffer, orgID, assetID int32) *CloudFlusher {
	return &CloudFlusher{
		logger: logger,
		buffer: buffer,
	}
}

// NewCloudFlusherWithProvisioning creates a CloudFlusher that reads cloud
// credentials and IDs from the ProvisioningService at startup.
func NewCloudFlusherWithProvisioning(logger *zap.Logger, buffer *TelemetryBuffer, provisioningSvc *ProvisioningService) *CloudFlusher {
	return &CloudFlusher{
		logger:          logger,
		buffer:          buffer,
		provisioningSvc: provisioningSvc,
	}
}

// Run waits until the agent is provisioned, then continuously flushes buffered
// logs to the cloud with exponential backoff (1s → 2s → 4s … capped at 60s).
// The backoff resets on each successful flush pass. Blocks until ctx is done.
func (f *CloudFlusher) Run(ctx context.Context) {
	if f.provisioningSvc == nil {
		// Test mode: no provisioning; nothing to do.
		return
	}

	var cloudHost string

	// Poll until provisioned.
	for {
		var enrolled bool
		cloudHost, _, _, enrolled = f.provisioningSvc.ProvisioningInfo()
		if enrolled {
			break
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}

	// NOTE: ProvisioningService stores the private key as []byte (zeroed on rotation).
	// Certs are re-fetched per dial so the keyData []byte copy is scoped to each
	// connection attempt and zeroed by dial on return, minimising the key-material
	// window. Crash dumps should be disabled on the device for defence-in-depth
	// (ulimit -c 0 / RLIMIT_CORE=0).

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		endpoint, err := telemetryCloudEndpoint(cloudHost, f.provisioningSvc.ProvisioningPrincipal(), os.Getenv("WENDY_TELEMETRY_URL"), os.Getenv("WENDY_DEVICE_CLOUD_URL"))
		if err != nil {
			f.logger.Warn("cloud flusher: invalid endpoint", zap.Error(err))
			return
		}
		certPEM, chainPEM, keyData := f.provisioningSvc.ProvisioningCerts()
		conn, err := f.dial(ctx, endpoint, certPEM, chainPEM, keyData)
		if err != nil {
			f.logger.Warn("cloud flusher: dial failed", zap.Error(err))
			f.sleep(ctx, attempt)
			attempt++
			continue
		}

		err = f.runOnce(ctx,
			collogspb.NewLogsServiceClient(conn),
			colmetricspb.NewMetricsServiceClient(conn),
			coltracepb.NewTraceServiceClient(conn),
		)
		conn.Close()

		if err != nil {
			f.logger.Warn("cloud flusher: flush failed", zap.String("endpoint", endpoint), zap.Error(err))
			f.sleep(ctx, attempt)
			if attempt < 6 { // 2^6 = 64s > 60s cap; further increments have no effect
				attempt++
			}
		} else {
			attempt = 0
			// Brief pause between successful passes to avoid busy-looping.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (f *CloudFlusher) sleep(ctx context.Context, attempt int) {
	backoff := time.Duration(math.Min(
		float64(time.Second)*math.Pow(2, float64(attempt)),
		float64(cloudFlusherMaxBackoff),
	))
	select {
	case <-time.After(backoff):
	case <-ctx.Done():
	}
}

// dial establishes a TLS 1.3 gRPC connection. keyData is zeroed on return as
// best-effort protection; crypto/tls may retain additional internal copies.
func (f *CloudFlusher) dial(ctx context.Context, host, certPEM, chainPEM string, keyData []byte) (*grpc.ClientConn, error) {
	defer clear(keyData)
	return cloudrelay.DialCloud(normalizeCloudHost(host), certPEM, chainPEM, keyData)
}

// runOnce performs a single flush pass over all three OTLP signals. For each
// signal it reads a batch of buffered frames, sanitizes each, and exports it
// via the matching OTLP collector route. A signal's cursor advances only after
// every frame in its batch is exported successfully (at-least-once delivery;
// the cloud tolerates duplicates). Org/asset identity is derived server-side
// from the mTLS client certificate, so no identifiers are sent in the request.
func (f *CloudFlusher) runOnce(ctx context.Context, logs collogspb.LogsServiceClient, metrics colmetricspb.MetricsServiceClient, traces coltracepb.TraceServiceClient) error {
	if f.buffer == nil {
		return nil
	}
	if err := f.flushSignal(SignalLogs, func(msg proto.Message) error {
		req := msg.(*collogspb.ExportLogsServiceRequest)
		sanitizeLogs(req)
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, err := logs.Export(callCtx, req)
		return telemetryExportError(err, "logs")
	}); err != nil {
		return err
	}
	if err := f.flushSignal(SignalMetrics, func(msg proto.Message) error {
		req := msg.(*colmetricspb.ExportMetricsServiceRequest)
		sanitizeMetrics(req)
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, err := metrics.Export(callCtx, req)
		return telemetryExportError(err, "metrics")
	}); err != nil {
		return err
	}
	return f.flushSignal(SignalTraces, func(msg proto.Message) error {
		req := msg.(*coltracepb.ExportTraceServiceRequest)
		sanitizeTraces(req)
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		_, err := traces.Export(callCtx, req)
		return telemetryExportError(err, "traces")
	})
}

// flushSignal reads one batch of buffered frames for sig, exports each via the
// provided callback, and advances the signal's cursor only if all exports
// succeed. On any export error the cursor is left untouched so the batch is
// retried (already-exported frames may be re-sent).
func (f *CloudFlusher) flushSignal(sig SignalType, export func(proto.Message) error) error {
	if f.buffer == nil {
		return nil
	}
	cursor := f.buffer.LoadCursor(sig)
	msgs, next, err := f.buffer.ReadFromCursor(sig, cursor, cloudFlusherFramesPerPass)
	if err != nil {
		return fmt.Errorf("cloud flusher: read %s from cursor: %w", sig, err)
	}
	if len(msgs) == 0 {
		return nil
	}
	for _, msg := range msgs {
		if err := export(msg); err != nil {
			return fmt.Errorf("cloud flusher: export %s: %w", sig, err)
		}
	}
	if err := f.buffer.SaveCursor(sig, next); err != nil {
		return fmt.Errorf("cloud flusher: save %s cursor: %w", sig, err)
	}
	return nil
}

func telemetryCloudEndpoint(cloudHost, principal, override, deviceOverride string) (string, error) {
	if override != "" {
		return cloudrelay.DeviceEndpoint(cloudHost, override)
	}
	if principal != "" {
		return cloudrelay.DeviceEndpoint(cloudHost, deviceOverride)
	}
	return normalizeCloudHost(cloudHost), nil
}
func telemetryExportError(err error, signal string) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("Cloud endpoint does not expose the OpenTelemetry %s Export service; verify the Cloud collector deployment or WENDY_TELEMETRY_URL: %w", signal, err)
	}
	return err
}
