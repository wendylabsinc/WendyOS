package cloudmcp

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"fmt"
	"github.com/google/uuid"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
)

type correlationKey struct{}

// Only generated identifiers and explicitly selected metadata enter telemetry.
// Never record request headers, OAuth tokens, JSON-RPC IDs, arguments, results,
// arbitrary client names, or error descriptions. Those may all contain secrets.
func (s *Server) observe(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(string)) {
	ctx, span := s.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	started := time.Now()
	fields := []any{"event", name, "correlation_id", ctx.Value(correlationKey{}), "trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String()}
	if c, ok := ctx.Value(callerKey{}).(caller); ok {
		fields = append(fields, "organization_id", c.org)
		if c.access.UserID != "" {
			fields = append(fields, "user_id", c.access.UserID, "service_subject", c.access.ServiceSubject)
			span.SetAttributes(attribute.String("wendy.user_id", c.access.UserID), attribute.String("wendy.service_subject", c.access.ServiceSubject))
		}
		span.SetAttributes(attribute.String("wendy.organization_id", c.org))
	}
	for _, a := range attrs {
		fields = append(fields, string(a.Key), a.Value.AsInterface())
	}
	s.logger.InfoContext(ctx, "hosted MCP event", append(fields, "phase", "started")...)
	return ctx, func(outcome string) {
		span.SetAttributes(attribute.String("wendy.outcome", outcome))
		if outcome == "error" || outcome == "denied" || outcome == "rejected" || outcome == "audit_unavailable" || outcome == "aborted" {
			span.SetStatus(codes.Error, outcome)
		}
		s.logger.InfoContext(ctx, "hosted MCP event", append(fields, "phase", "finished", "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())...)
		span.End()
	}
}

func (s *Server) observedTool(name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		c, _ := ctx.Value(callerKey{}).(caller)
		ctx, end, auditErr := s.audited(ctx, "mcp.tool", c.access, attribute.String("mcp.tool.name", name))
		if auditErr != nil {
			return mcp.NewToolResultError("audit unavailable"), nil
		}
		outcome := "error"
		defer func() { end(outcome) }()
		result, err = next(ctx, request)
		if err == nil && result != nil && !result.IsError {
			outcome = "ok"
		}
		return
	}
}

// WithTelemetry allows embedders and tests to supply an exporter and log sink.
// Audits in Cloud are independent of this best-effort diagnostic telemetry.
func WithTelemetry(logger *slog.Logger, provider trace.TracerProvider) func(*Server) {
	return func(s *Server) {
		if logger != nil {
			s.logger = logger
		}
		if provider != nil {
			s.tracer = provider.Tracer("wendy.cloudmcp")
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	correlation := uuid.NewString()
	// Untrusted clients cannot choose our trace IDs or sampling policy. Preserve a
	// valid upstream trace only as a link; never extract baggage or tracestate.
	parent := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(r.Header))
	opts := []trace.SpanStartOption{trace.WithNewRoot(), trace.WithSpanKind(trace.SpanKindServer)}
	if sc := trace.SpanContextFromContext(parent); sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: sc.TraceID(), SpanID: sc.SpanID(), Remote: true})}))
	}
	ctx, span := s.tracer.Start(context.WithValue(r.Context(), correlationKey{}, correlation), "mcp.inbound", opts...)
	defer span.End()
	span.SetAttributes(attribute.String("wendy.correlation_id", correlation))
	ctx, end := s.observe(ctx, "mcp.http")
	w.Header().Set("X-Request-ID", correlation)
	writer := &observedWriter{ResponseWriter: w}
	outcome := "aborted"
	defer func() {
		if writer.status == 0 {
			writer.status = http.StatusOK
		}
		span.SetAttributes(attribute.Int("http.response.status_code", writer.status))
		s.logger.InfoContext(ctx, "hosted MCP HTTP response", "event", "mcp.http.response", "correlation_id", correlation, "trace_id", span.SpanContext().TraceID().String(), "http_status", writer.status)
		end(outcome)
	}()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writer.Header().Set("Retry-After", "5")
		http.Error(writer, "server busy", http.StatusServiceUnavailable)
		outcome = "rejected"
		return
	}
	s.serveHTTP(writer, r.WithContext(ctx))
	if writer.status >= 400 {
		outcome = "rejected"
	} else {
		outcome = "ok"
	}
}

func (s *Server) protocolHooks() *server.Hooks {
	hooks := &server.Hooks{}
	record := func(ctx context.Context, method mcp.MCPMethod, outcome string) {
		// Unknown protocol methods may contain arbitrary client data.
		name := "unknown"
		switch string(method) {
		case "initialize", "ping", "tools/list", "tools/call", "notifications/initialized", "notifications/cancelled":
			name = string(method)
		}
		trace.SpanFromContext(ctx).AddEvent("mcp.protocol", trace.WithAttributes(attribute.String("mcp.method.name", name), attribute.String("wendy.outcome", outcome)))
		s.logger.InfoContext(ctx, "hosted MCP protocol event", "event", "mcp.protocol", "method", name, "outcome", outcome, "correlation_id", ctx.Value(correlationKey{}), "trace_id", trace.SpanContextFromContext(ctx).TraceID().String())
	}
	hooks.AddBeforeAny(func(ctx context.Context, _ any, method mcp.MCPMethod, _ any) { record(ctx, method, "started") })
	hooks.AddOnSuccess(func(ctx context.Context, _ any, method mcp.MCPMethod, _ any, _ any) { record(ctx, method, "completed") })
	hooks.AddOnError(func(ctx context.Context, _ any, method mcp.MCPMethod, _ any, _ error) { record(ctx, method, "error") })
	return hooks
}

type observedWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *observedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}
func (w *observedWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *observedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (s *Server) connect(ctx context.Context, access Access, device string) (*grpcclient.AgentConnection, func(), error) {
	ctx, end, auditErr := s.audited(ctx, "mcp.device_connection", access, attribute.String("wendy.device_id", device))
	if auditErr != nil {
		return nil, nil, auditErr
	}
	conn, err := s.connector.Connect(deviceTraceContext(ctx), access, device)
	if err != nil {
		end("error")
		return nil, nil, err
	}
	// grpc.NewClient is lazy. Only report a connected transport after its
	// handshake reaches Ready; a constructed client is not connection evidence.
	ready, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn.Conn.Connect()
	for state := conn.Conn.GetState(); state != connectivity.Ready; state = conn.Conn.GetState() {
		if state == connectivity.Shutdown || !conn.Conn.WaitForStateChange(ready, state) {
			_ = conn.Close()
			end("error")
			return nil, nil, fmt.Errorf("device connection unavailable")
		}
	}
	trace.SpanFromContext(ctx).AddEvent("connected")
	s.logger.InfoContext(ctx, "hosted MCP device connected", "event", "mcp.device_connection", "phase", "connected", "correlation_id", ctx.Value(correlationKey{}), "trace_id", trace.SpanContextFromContext(ctx).TraceID().String(), "span_id", trace.SpanContextFromContext(ctx).SpanID().String(), "organization_id", access.OrganizationID, "user_id", access.UserID, "service_subject", access.ServiceSubject, "device_id", device)
	return conn, func() { _ = conn.Close(); end("ok") }, nil
}

// Forward only server-generated tracing metadata. Identity and authority still
// come from the authenticated transport, never from these diagnostic headers.
func deviceTraceContext(ctx context.Context) context.Context {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	pairs := []string{}
	if value := carrier.Get("traceparent"); value != "" {
		pairs = append(pairs, "traceparent", value)
	}
	if value, ok := ctx.Value(correlationKey{}).(string); ok {
		pairs = append(pairs, "x-correlation-id", value)
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}
