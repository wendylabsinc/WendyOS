package interceptor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Called only after mTLS authorization. Correlation headers are untrusted hints;
// the certificate fingerprint is the authenticated peer, never an asserted user.
// Ordinary calls without tracing metadata remain silent to avoid logging every
// telemetry poll. A caller can omit hints; this is not mandatory grant auditing.
func auditTracedRPC(ctx context.Context, logger *zap.Logger, method string) func(error) {
	md, _ := metadata.FromIncomingContext(ctx)
	ids, parents := md.Get("x-correlation-id"), md.Get("traceparent")
	if len(ids) != 1 || len(parents) != 1 {
		return func(error) {}
	}
	id, err := uuid.Parse(ids[0])
	if err != nil || id == uuid.Nil || id.String() != ids[0] {
		return func(error) {}
	}
	parsed := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": parents[0]})
	sc := trace.SpanContextFromContext(parsed)
	if !sc.IsValid() {
		return func(error) {}
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return func(error) {}
	}
	tls, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tls.State.PeerCertificates) == 0 {
		return func(error) {}
	}
	fingerprint := sha256.Sum256(tls.State.PeerCertificates[0].Raw)
	fields := []zap.Field{
		zap.String("event", "device.rpc"), zap.String("evidence", "device_observed"),
		zap.String("correlation_id", id.String()), zap.String("correlation_source", "peer_hint"),
		zap.String("trace_id", sc.TraceID().String()), zap.String("parent_span_id", sc.SpanID().String()),
		zap.String("method", method), zap.String("peer_certificate_sha256", hex.EncodeToString(fingerprint[:])),
	}
	started := time.Now()
	logger.Info("traced device RPC", append(fields, zap.String("phase", "started"))...)
	return func(err error) {
		logger.Info("traced device RPC", append(fields, zap.String("phase", "finished"), zap.String("grpc_status", status.Code(err).String()), zap.Int64("duration_ms", time.Since(started).Milliseconds()))...)
	}
}
