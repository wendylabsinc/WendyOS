package services

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

const (
	maxOTELHTTPBodySize           = 10 * 1024 * 1024 // 10 MB decompressed
	maxOTELHTTPCompressedBodySize = 12 * 1024 * 1024 // 12 MB compressed — gzip NoCompression adds ~0.015% framing overhead,
	// so the compressed cap must exceed the decompressed cap by at least that margin to avoid
	// rejecting valid 10 MB payloads with poor compression ratios.
)

var (
	errBodyTooLarge           = fmt.Errorf("request body exceeds %d bytes", maxOTELHTTPBodySize)
	errCompressedBodyTooLarge = fmt.Errorf("compressed request body exceeds %d bytes", maxOTELHTTPCompressedBodySize)
)

// OTELHTTPReceiver serves OTLP data over HTTP/protobuf on port 4318.
// Many OTEL SDKs (including the Python SDK) default to HTTP/protobuf export
// rather than gRPC on port 4317.
type OTELHTTPReceiver struct {
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	server      *http.Server
}

// NewOTELHTTPReceiver creates a new OTELHTTPReceiver.
func NewOTELHTTPReceiver(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *OTELHTTPReceiver {
	r := &OTELHTTPReceiver{
		logger:      logger,
		broadcaster: broadcaster,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", r.handleLogs)
	mux.HandleFunc("POST /v1/metrics", r.handleMetrics)
	mux.HandleFunc("POST /v1/traces", r.handleTraces)

	r.server = &http.Server{
		Handler: mux,
	}

	return r
}

// Serve starts serving HTTP requests on the given listener.
func (r *OTELHTTPReceiver) Serve(listener net.Listener) error {
	return r.server.Serve(listener)
}

// Shutdown gracefully shuts down the HTTP server.
func (r *OTELHTTPReceiver) Shutdown(ctx context.Context) error {
	return r.server.Shutdown(ctx)
}

func (r *OTELHTTPReceiver) handleLogs(w http.ResponseWriter, req *http.Request) {
	body, err := r.readBody(req)
	if err != nil {
		r.logger.Warn("Failed to read OTLP logs request body", zap.Error(err))
		r.writeBodyError(w, err)
		return
	}

	var logsReq otelpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(body, &logsReq); err != nil {
		r.logger.Warn("Failed to unmarshal OTLP logs request", zap.Error(err))
		http.Error(w, "failed to unmarshal protobuf", http.StatusBadRequest)
		return
	}

	r.broadcaster.PublishLogs(&logsReq)
	w.WriteHeader(http.StatusOK)
}

func (r *OTELHTTPReceiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	body, err := r.readBody(req)
	if err != nil {
		r.logger.Warn("Failed to read OTLP metrics request body", zap.Error(err))
		r.writeBodyError(w, err)
		return
	}

	var metricsReq otelpb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &metricsReq); err != nil {
		r.logger.Warn("Failed to unmarshal OTLP metrics request", zap.Error(err))
		http.Error(w, "failed to unmarshal protobuf", http.StatusBadRequest)
		return
	}

	r.broadcaster.PublishMetrics(&metricsReq)
	w.WriteHeader(http.StatusOK)
}

func (r *OTELHTTPReceiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	body, err := r.readBody(req)
	if err != nil {
		r.logger.Warn("Failed to read OTLP traces request body", zap.Error(err))
		r.writeBodyError(w, err)
		return
	}

	var traceReq otelpb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &traceReq); err != nil {
		r.logger.Warn("Failed to unmarshal OTLP traces request", zap.Error(err))
		http.Error(w, "failed to unmarshal protobuf", http.StatusBadRequest)
		return
	}

	r.broadcaster.PublishTraces(&traceReq)
	w.WriteHeader(http.StatusOK)
}

func (r *OTELHTTPReceiver) writeBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) || errors.Is(err, errCompressedBodyTooLarge) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	} else {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
	}
}

// isGzipEncoded reports whether the request uses gzip content encoding.
// Content-Encoding is case-insensitive and may list multiple codings; we only
// support a single "gzip" token (the common OTLP SDK case). Multi-coding values
// are not decompressed and will fail protobuf unmarshalling, not silently corrupt.
func isGzipEncoded(req *http.Request) bool {
	enc := strings.TrimSpace(req.Header.Get("Content-Encoding"))
	if enc == "" {
		return false
	}
	// Handle comma-separated codings: only treat as gzip if the sole token is "gzip".
	parts := strings.SplitN(enc, ",", 2)
	return len(parts) == 1 && strings.EqualFold(strings.TrimSpace(parts[0]), "gzip")
}

func (r *OTELHTTPReceiver) readBody(req *http.Request) ([]byte, error) {
	reader := io.Reader(req.Body)
	if isGzipEncoded(req) {
		// Buffer the compressed bytes so an over-limit request returns a clear
		// 413 rather than an opaque gzip decode error. The compressed cap
		// matches the decompressed cap so valid payloads with poor compression
		// ratios are not incorrectly rejected.
		compressed, err := io.ReadAll(io.LimitReader(req.Body, maxOTELHTTPCompressedBodySize+1))
		if err != nil {
			return nil, err
		}
		if int64(len(compressed)) > maxOTELHTTPCompressedBodySize {
			return nil, errCompressedBodyTooLarge
		}
		gz, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	limited := io.LimitReader(reader, maxOTELHTTPBodySize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxOTELHTTPBodySize {
		return nil, errBodyTooLarge
	}
	return body, nil
}
