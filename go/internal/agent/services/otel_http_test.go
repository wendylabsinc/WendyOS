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

	// Incompressible (random) payload so the gzip stream stays above the
	// compressed cap while decompressing to well under the 10 MiB decompressed
	// cap — proving the compressed limit is enforced independently.
	raw := make([]byte, maxOTELHTTPCompressedBodySize+64*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}
	if buf.Len() <= maxOTELHTTPCompressedBodySize {
		t.Fatalf("test setup: compressed size %d not above cap %d", buf.Len(), maxOTELHTTPCompressedBodySize)
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

	// Random payload that gzips to just under the compressed cap. It is not
	// valid protobuf, so a 400 (decode error) rather than a 413 confirms the
	// compressed-size gate let an under-limit request through.
	raw := make([]byte, maxOTELHTTPCompressedBodySize/2)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip.Write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close: %v", err)
	}
	if buf.Len() > maxOTELHTTPCompressedBodySize {
		t.Fatalf("test setup: compressed size %d exceeds cap %d", buf.Len(), maxOTELHTTPCompressedBodySize)
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/metrics", &buf)
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
