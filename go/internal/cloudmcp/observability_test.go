package cloudmcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

type auditBackend struct {
	*fakeBackend
	events         []GatewayEvent
	fail           string
	completionLive bool
}

func (b *auditBackend) Record(ctx context.Context, _ Access, event GatewayEvent) error {
	b.events = append(b.events, event)
	if event.Phase == "finished" {
		b.completionLive = ctx.Err() == nil
	}
	if event.Event == b.fail {
		return errors.New("secret-backend-error")
	}
	return nil
}

func observedFixture(t *testing.T) (*Server, *auditBackend, *fakeConnector, *bytes.Buffer, *tracetest.InMemoryExporter) {
	t.Helper()
	s, base, connector := fixture(t)
	base.access.DecisionID = uuid.NewString()
	b := &auditBackend{fakeBackend: base}
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	logs := new(bytes.Buffer)
	s.backend = b
	s.logger = slog.New(slog.NewJSONHandler(logs, nil))
	s.tracer = provider.Tracer("test")
	return s, b, connector, logs, exporter
}

func TestInboundTracingIgnoresClientIdentityAndSecrets(t *testing.T) {
	s, b, _, logs, exporter := observedFixture(t)
	r := httptest.NewRequest(http.MethodPost, ProductionURL+"/orgs/"+testOrg+"/mcp?secret=query", strings.NewReader(`{"jsonrpc":"2.0","id":"secret-request-id","method":"tools/call","params":{"name":"device_list","arguments":{"secret":"secret-argument"}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer user-token")
	r.Header.Set("X-Request-ID", "secret-request-header")
	r.Header.Set("User-Agent", "secret-user-agent")
	r.Header.Set("Baggage", "secret-baggage")
	r.Header.Set("Traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-00")
	r.Header.Set("Tracestate", "vendor=secret-tracestate")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !canonicalUUID(w.Header().Get("X-Request-ID")) {
		t.Fatal(w.Code, w.Header())
	}
	if len(b.events) != 2 || b.events[0].Phase != "started" || b.events[1].Outcome != "ok" {
		t.Fatalf("bad events: %+v", b.events)
	}
	if b.events[0].TraceID != b.events[1].TraceID || b.events[0].SpanID != b.events[1].SpanID {
		t.Fatal("lost event pairing")
	}
	spans := exporter.GetSpans()
	if len(spans) < 4 {
		t.Fatalf("missing spans: %d", len(spans))
	}
	var rootFound bool
	for _, span := range spans {
		if span.SpanContext.TraceID().String() == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatal("trusted inbound trace ID")
		}
		if span.Name == "mcp.inbound" {
			rootFound = true
			if span.Parent.IsValid() || len(span.Links) != 1 || span.Links[0].SpanContext.TraceState().String() != "" {
				t.Fatal("bad root/link")
			}
		}
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.Emit(), "secret-") || strings.Contains(attr.Value.Emit(), "user-token") {
				t.Fatalf("leaked attribute: %v", attr.Key)
			}
		}
	}
	if !rootFound {
		t.Fatal("missing inbound span")
	}
	for _, secret := range []string{"secret-", "user-token", "secret=query"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret leaked to logs")
		}
	}
	if !strings.Contains(logs.String(), w.Header().Get("X-Request-ID")) {
		t.Fatal("missing correlation")
	}
}

func TestAuditFailurePreventsDeviceConnection(t *testing.T) {
	s, b, connector, logs, _ := observedFixture(t)
	b.fail = "mcp.device_connection"
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_rpc","arguments":{"device_id":"` + testDevice + `","method":"` + agentpb.WendyAgentService_GetAgentVersion_FullMethodName + `"}}}`
	w := request(s, body, "Bearer user-token")
	if connector.calls != 0 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("unaudited device work executed", connector.calls, w.Body.String())
	}
	if strings.Contains(logs.String(), "secret-backend-error") {
		t.Fatal("error details leaked")
	}
}

func TestCompletionAuditSurvivesClientCancellation(t *testing.T) {
	s, b, _, _, _ := observedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, finish, err := s.audited(ctx, "mcp.tool", b.access)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	finish("cancelled")
	if !b.completionLive || len(b.events) != 2 {
		t.Fatal("completion lost on disconnect")
	}
}

func TestInvalidHTTPRequestsAreTracedWithoutIdentity(t *testing.T) {
	s, b, _, logs, exporter := observedFixture(t)
	w := request(s, `{}`, "")
	if w.Code != 401 || len(b.events) != 0 {
		t.Fatal(w.Code, b.events)
	}
	if len(exporter.GetSpans()) != 2 || !strings.Contains(logs.String(), "rejected") || strings.Contains(logs.String(), "user_id") {
		t.Fatal("incorrect unauthenticated telemetry")
	}
}

func TestDeviceTraceDoesNotForwardBaggage(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.WithValue(context.Background(), correlationKey{}, uuid.NewString()), sc)
	md, ok := metadata.FromOutgoingContext(deviceTraceContext(ctx))
	if !ok || len(md.Get("traceparent")) != 1 || len(md.Get("x-correlation-id")) != 1 || len(md.Get("baggage")) != 0 {
		t.Fatal("bad diagnostic metadata", md)
	}
}
