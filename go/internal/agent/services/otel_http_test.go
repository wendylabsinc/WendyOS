package services

import (
	"bytes"
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
