package cloudmcp

import (
	"context"
	"github.com/coder/websocket"
	"go.opentelemetry.io/otel/attribute"
	"io"
	"net"
	"net/http"
	"time"
)

// ServiceConnector resolves symbols through Cloud's signed tunnel catalog.
// wendy-agent is deliberately excluded from raw forwarding: its gRPC requests
// must pass through this server's per-method authorization boundary.
type ServiceConnector interface {
	OpenService(context.Context, Access, string, string) (net.Conn, error)
}

func (b *CloudBackend) OpenService(ctx context.Context, a Access, device, service string) (net.Conn, error) {
	session := b.sessions[a.OrganizationID]
	if session == nil || !a.permits(a.OrganizationID) || !allowedService(service) {
		return nil, io.ErrClosedPipe
	}
	return session.OpenService(ctx, a.TenantID, a.ServiceSubject, device, service)
}
func allowedService(service string) bool {
	return service == "ssh" || service == "wendy-registry" || service == "wendy-registry-darwin"
}
func (s *Server) serveServiceTunnel(w http.ResponseWriter, r *http.Request, device, service string, a Access) {
	connector, ok := s.connector.(ServiceConnector)
	if !ok || !allowedService(service) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), minTime(a.ExpiresAt, time.Now().Add(time.Hour)))
	defer cancel()
	ctx, stop := s.watchAuthorization(ctx, device, "service:"+service, a)
	defer stop()
	ctx, finish, auditErr := s.audited(ctx, "mcp.service_connection", a, attribute.String("wendy.device_id", device), attribute.String("wendy.service", service))
	if auditErr != nil {
		http.Error(w, "audit unavailable", http.StatusServiceUnavailable)
		return
	}
	outcome := "error"
	defer func() { finish(outcome) }()
	upstream, err := connector.OpenService(deviceTraceContext(ctx), a, device, service)
	if err != nil {
		http.Error(w, "device service unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	outcome = "closed"
	ws.SetReadLimit(1 << 20)
	downstream := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer downstream.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			upstream.Close()
			downstream.Close()
		case <-done:
		}
	}()
	defer close(done)
	go func() { _, _ = io.Copy(upstream, downstream); cancel() }()
	_, _ = io.Copy(downstream, upstream)
}
