package cloudmcp

import (
	"context"
	"github.com/coder/websocket"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type echoServiceConnector struct{ fakeConnector }

func (*echoServiceConnector) OpenService(ctx context.Context, a Access, device, service string) (net.Conn, error) {
	local, peer := net.Pipe()
	go func() { defer peer.Close(); _, _ = io.Copy(peer, peer) }()
	return local, nil
}
func TestNamedServiceTunnelTransportsBytes(t *testing.T) {
	var gateway *Server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gateway.ServeHTTP(w, r) }))
	defer server.Close()
	var err error
	gateway, err = New(server.URL, &tunnelBackend{}, &echoServiceConnector{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := server.URL + "/orgs/" + testOrg + "/devices/" + testDevice + "/services/ssh/tunnel"
	ws, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": {"Bearer user-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "hello" {
		t.Fatal(string(data), err)
	}
}
func TestRawAgentAndArbitraryServicesAreRefused(t *testing.T) {
	server, _, _ := fixture(t)
	for _, service := range []string{"wendy-agent", "127.0.0.1:50052", "tcp-8080", ".."} {
		req := httptest.NewRequest(http.MethodGet, ProductionURL+"/orgs/"+testOrg+"/devices/"+testDevice+"/services/"+service+"/tunnel", nil)
		req.Header.Set("Authorization", "Bearer user-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatal(service, response.Code)
		}
	}
}
