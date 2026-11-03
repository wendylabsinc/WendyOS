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
	"google.golang.org/grpc/metadata"
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
	// flush pass merges into one bounded Export RPC before looping.
	cloudFlusherFramesPerPass = 50
	cloudFlusherMaxBatchBytes = 2 << 20
)

// CloudFlusher reads buffered OTLP frames from TelemetryBuffer via ReadFromCursor
// and re-exports them to v1 Cloud or a configured OTLP collector's routes
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

// Run uses the enrolled v1 OTLP endpoint, or WENDY_TELEMETRY_URL for PKI,
// then waits until the agent is provisioned
// and continuously flushes buffered telemetry with exponential backoff
// (1s → 2s → 4s … capped at 60s). Without a collector, telemetry stays local.
// The backoff resets on each successful flush pass. Blocks until ctx is done.
func (f *CloudFlusher) Run(ctx context.Context) {
	endpoint, err := telemetryCloudEndpoint(os.Getenv("WENDY_TELEMETRY_URL"))
	if err != nil {
		f.logger.Warn("cloud flusher: invalid WENDY_TELEMETRY_URL", zap.Error(err))
		return
	}
	if f.provisioningSvc == nil {
		// Test mode: no provisioning; nothing to do.
		return
	}

	// Poll until provisioned.
	for {
		_, _, _, enrolled := f.provisioningSvc.ProvisioningInfo()
		if enrolled {
			break
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}

	if endpoint == "" {
		enrollment, err := f.provisioningSvc.CloudEnrollment()
		if err != nil {
			f.logger.Error("cloud flusher: invalid enrollment", zap.Error(err))
			return
		}
		if enrollment.Mode != CloudLegacy {
			f.logger.Info("cloud flusher: WENDY_TELEMETRY_URL is not set; telemetry uploads are disabled and telemetry remains available locally")
			return
		}
		endpoint, err = telemetryCloudEndpoint(enrollment.Host)
		if err != nil {
			f.logger.Error("cloud flusher: invalid legacy endpoint", zap.Error(err))
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
	if f.provisioningSvc != nil {
		enrollment, err := f.provisioningSvc.CloudEnrollment()
		if err != nil {
			return nil, err
		}
		if enrollment.Mode == CloudLegacy {
			opts, proof, err := NewTunnelBrokerClient(f.logger, host, certPEM, string(keyData), chainPEM, 0).buildDialOpts()
			if err != nil {
				return nil, err
			}
			opts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, callOpts ...grpc.CallOption) error {
				md, err := proof(method)
				if err != nil {
					return err
				}
				existing, _ := metadata.FromOutgoingContext(ctx)
				return invoke(metadata.NewOutgoingContext(ctx, metadata.Join(existing, md)), method, req, reply, cc, callOpts...)
			}))
			return grpc.NewClient(normalizeCloudHost(host), opts...)
		}
	}
	return cloudrelay.DialCloud(normalizeCloudHost(host), certPEM, chainPEM, keyData)
}

// runOnce performs a single flush pass over all three OTLP signals. For each
// signal it reads a batch of buffered frames, sanitizes each, and exports it
// via the matching OTLP collector route. A signal's cursor advances only after
// its bounded batch is exported successfully (at-least-once delivery;
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
	// Merge OTLP frames into one bounded export. Replaying a backlog one RPC
	// per frame exhausts the v1 per-device rate limit before a batch can finish,
	// so its cursor never advances and the same prefix is replayed forever.
	count := cloudFlusherFramesPerPass
	var msgs []proto.Message
	var next flushCursor
	for {
		var err error
		msgs, next, err = f.buffer.ReadFromCursor(sig, cursor, count)
		if err != nil {
			return fmt.Errorf("cloud flusher: read %s from cursor: %w", sig, err)
		}
		if len(msgs) == 0 {
			return nil
		}
		size := 0
		for _, msg := range msgs {
			size += proto.Size(msg)
		}
		if size <= cloudFlusherMaxBatchBytes || count == 1 {
			break
		}
		count = max(1, count/2)
	}
	batch := proto.Clone(msgs[0])
	for _, msg := range msgs[1:] {
		proto.Merge(batch, msg)
	}
	if err := export(batch); err != nil {
		return fmt.Errorf("cloud flusher: export %s: %w", sig, err)
	}
	if err := f.buffer.SaveCursor(sig, next); err != nil {
		return fmt.Errorf("cloud flusher: save %s cursor: %w", sig, err)
	}
	return nil
}

func telemetryCloudEndpoint(endpoint string) (string, error) {
	// An explicit collector is independently configured for PKI enrollment.
	// Run selects the enrolled v1 endpoint only for legacy Cloud, which still
	// serves OTLP; this parser must not remap it to a v2 devices listener.
	if endpoint == "" {
		return "", nil
	}
	return cloudrelay.DeviceEndpoint("", endpoint)
}

func telemetryExportError(err error, signal string) error {
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("Telemetry endpoint does not expose the OpenTelemetry %s Export service; verify the collector deployment or WENDY_TELEMETRY_URL: %w", signal, err)
	}
	return err
}
