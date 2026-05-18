package services

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

func TestOTELHTTPReceiver_HandleLogs(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	id, ch := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(id)

	req := &otelpb.ExportLogsServiceRequest{
		ResourceLogs: []*otelpb.ResourceLogs{
			{
				ScopeLogs: []*otelpb.ScopeLogs{
					{
						LogRecords: []*otelpb.LogRecord{
							{
								SeverityNumber: otelpb.SeverityNumber_SEVERITY_NUMBER_INFO,
								Body: &otelpb.AnyValue{
									Value: &otelpb.AnyValue_StringValue{StringValue: "test log"},
								},
							},
						},
					},
				},
			},
		},
	}

	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch:
		if len(got.ResourceLogs) != 1 {
			t.Errorf("expected 1 ResourceLogs, got %d", len(got.ResourceLogs))
		}
	case <-time.After(time.Second):
		t.Error("did not receive published log")
	}
}

func TestOTELHTTPReceiver_HandleMetrics(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	id, ch := broadcaster.SubscribeMetrics()
	defer broadcaster.UnsubscribeMetrics(id)

	req := &otelpb.ExportMetricsServiceRequest{}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case <-ch:
		// OK
	case <-time.After(time.Second):
		t.Error("did not receive published metrics")
	}
}

func TestOTELHTTPReceiver_HandleTraces(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	id, ch := broadcaster.SubscribeTraces()
	defer broadcaster.UnsubscribeTraces(id)

	req := &otelpb.ExportTraceServiceRequest{}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case <-ch:
		// OK
	case <-time.After(time.Second):
		t.Error("did not receive published traces")
	}
}

func TestOTELHTTPReceiver_HandleLogsGzip(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	id, ch := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(id)

	req := &otelpb.ExportLogsServiceRequest{
		ResourceLogs: []*otelpb.ResourceLogs{
			{
				ScopeLogs: []*otelpb.ScopeLogs{
					{
						LogRecords: []*otelpb.LogRecord{
							{
								SeverityNumber: otelpb.SeverityNumber_SEVERITY_NUMBER_INFO,
								Body: &otelpb.AnyValue{
									Value: &otelpb.AnyValue_StringValue{StringValue: "gzip log"},
								},
							},
						},
					},
				},
			},
		},
	}

	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/logs", &buf)
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-ch:
		if len(got.ResourceLogs) != 1 {
			t.Errorf("expected 1 ResourceLogs, got %d", len(got.ResourceLogs))
		}
		logs := got.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		if v := logs.Body.GetStringValue(); v != "gzip log" {
			t.Errorf("expected body %q, got %q", "gzip log", v)
		}
	case <-time.After(time.Second):
		t.Error("did not receive published log")
	}
}

func TestOTELHTTPReceiver_InvalidProtobuf(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader([]byte("not protobuf")))
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid protobuf, got %d", w.Code)
	}
}

func TestOTELHTTPReceiver_BodyTooLarge(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	// Create a body larger than 10MB.
	largeBody := make([]byte, maxOTELHTTPBodySize+100)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(largeBody))
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", w.Code)
	}
}

func TestOTELHTTPReceiver_CompressedBodyTooLarge(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	// Build a valid gzip stream whose compressed size exceeds the cap while its
	// decompressed content is exactly maxOTELHTTPBodySize (not over). With
	// gzip.NoCompression the output equals the input plus gzip envelope overhead
	// (~1 KiB for 10 MiB), so compressed > maxOTELHTTPCompressedBodySize but
	// decompressed == maxOTELHTTPBodySize. Without the compressed-size guard the
	// gzip.Reader would succeed and the decompressed limit would not fire (the
	// check is strictly greater-than), proving the two guards are independent.
	decompressed := make([]byte, maxOTELHTTPBodySize)
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	if err != nil {
		t.Fatalf("gzip.NewWriterLevel: %v", err)
	}
	if _, err := gz.Write(decompressed); err != nil {
		t.Fatalf("gz.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	if buf.Len() <= maxOTELHTTPCompressedBodySize {
		t.Fatalf("precondition: gzip output (%d) must exceed compressed cap (%d)", buf.Len(), maxOTELHTTPCompressedBodySize)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", &buf)
	httpReq.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized compressed body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOTELHTTPReceiver_CompressedBodyWithinLimitNotRejectedForSize(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELHTTPReceiver(zap.NewNop(), broadcaster)

	// Send raw bytes under the compressed cap. The compressed-size check lets
	// the request through; gzip.NewReader then rejects the non-gzip bytes,
	// so a 400 (not 413) confirms the size gate did not fire.
	body := make([]byte, maxOTELHTTPCompressedBodySize/2)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	httpReq.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	receiver.server.Handler.ServeHTTP(w, httpReq)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("under-limit compressed body wrongly rejected as too large")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (invalid protobuf) for under-limit body, got %d: %s", w.Code, w.Body.String())
	}
}
