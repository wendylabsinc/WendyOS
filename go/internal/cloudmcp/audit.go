package cloudmcp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// GatewayEvent is a bounded, secret-free report tied to a Cloud decision. It is
// not independent evidence that the device executed an operation.
type GatewayEvent struct {
	DecisionID string `json:"decision_id"`
	EventID    string `json:"event_id"`
	Event      string `json:"event"`
	Phase      string `json:"phase"`
	Outcome    string `json:"outcome"`
	Tool       string `json:"tool_name,omitempty"`
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id"`
	DurationMS int64  `json:"duration_ms"`
}

func (b *CloudBackend) Record(ctx context.Context, access Access, event GatewayEvent) error {
	if !canonicalUUID(access.OrganizationID) || !canonicalUUID(event.DecisionID) {
		return fmt.Errorf("missing audit decision")
	}
	_, err := b.request(ctx, access.OrganizationID, "events", event)
	return err
}

// An authoritative start append must succeed before device work. Finishing uses
// a separate bounded context so client disconnects do not erase the report.
// Completion delivery can fail after an effect; retain the unmatched start and
// emit a diagnostic failure rather than claiming the operation was rolled back.
func (s *Server) audited(ctx context.Context, name string, access Access, attrs ...attribute.KeyValue) (context.Context, func(string), error) {
	ctx, end := s.observe(ctx, name, attrs...)
	auditor := s.backend
	event := GatewayEvent{DecisionID: access.DecisionID, EventID: uuid.NewString(), Event: name, Phase: "started", Outcome: "pending", TraceID: trace.SpanContextFromContext(ctx).TraceID().String(), SpanID: trace.SpanContextFromContext(ctx).SpanID().String()}
	for _, a := range attrs {
		if a.Key == "mcp.tool.name" {
			event.Tool = a.Value.AsString()
		}
	}
	if err := auditor.Record(ctx, access, event); err != nil {
		end("audit_unavailable")
		return ctx, nil, fmt.Errorf("audit unavailable")
	}
	started := time.Now()
	return ctx, func(outcome string) {
		event.EventID = uuid.NewString()
		event.Phase, event.Outcome = "finished", outcome
		event.DurationMS = time.Since(started).Milliseconds()
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := auditor.Record(finish, access, event); err != nil {
			trace.SpanFromContext(ctx).AddEvent("audit_delivery_failed")
			s.logger.ErrorContext(finish, "hosted MCP completion audit unavailable", "event", "audit_delivery_failed", "event_id", event.EventID, "decision_id", event.DecisionID, "correlation_id", ctx.Value(correlationKey{}), "trace_id", event.TraceID, "span_id", event.SpanID)
		}
		end(outcome)
	}, nil
}
