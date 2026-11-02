package commands

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Normal run readiness must use the selected operator endpoint's authenticated
// app stream, without discovering a device LAN address or requiring OS routes.
func TestOperatorMeshRunHTTPReadinessUsesSelectedAppStream(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	device, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	const port = 8080
	conn := &grpcclient.AgentConnection{Host: "mesh://" + tenant.String() + "/" + device.UUID().String(), OperatorMeshTarget: device, Reconnect: neverReconnect, AppDialer: func(ctx context.Context, selected uint16) (net.Conn, error) {
		if selected != port {
			t.Errorf("selected port=%d", selected)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	cfg := &appconfig.AppConfig{AppID: "normal-http", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: port}}}
	host, url, ok := hostSideAppAddress(context.Background(), conn, cfg)
	if !ok || host != device.MeshAddress().String() || url != "" {
		t.Fatalf("userspace app access host=%s url=%s ready=%v", host, url, ok)
	}
	if isCloudConnection(conn) {
		t.Fatal("operator reconnect was classified as Cloud")
	}
	readiness := effectiveReadiness(cfg)
	httpPort := cloudHTTPReadinessPort(conn, cfg, readiness, host)
	if httpPort != port {
		t.Fatal("operator HTTP app did not select request-level readiness")
	}
	if err := waitForReadinessWithin(context.Background(), readiness, host, httpPort, time.Second, conn); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		if request.Method != http.MethodHead || !request.Close || !strings.Contains(request.Host, device.MeshAddress().String()) {
			t.Fatalf("normal readiness request %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("selected app stream received no request")
	}
}

func TestOperatorMeshBrowserForwardRetainsPathAndConnectionOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.RequestURI())) }))
	defer server.Close()
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	device, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	conn := &grpcclient.AgentConnection{OperatorMeshTarget: device, AppDialer: func(ctx context.Context, port uint16) (net.Conn, error) {
		if port != 8080 {
			t.Errorf("forward selected port=%d", port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer conn.Close()
	cfg := &appconfig.AppConfig{AppID: "normal-http", Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://${WENDY_HOSTNAME}:8080/ui?q=ok#section"}}}
	before := len(operatorBrowserSlots)
	address, err := prepareOperatorBrowserURL(ctx, conn, cfg, device.MeshAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(address, "http://127.0.0.1:") || !strings.HasSuffix(address, "/ui?q=ok#section") {
		t.Fatalf("local viewer URL=%s", address)
	}
	if cfg.Hooks.PostStart.OpenURL != "http://${WENDY_HOSTNAME}:8080/ui?q=ok#section" {
		t.Fatal("viewer mutated the application description")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	response, err := client.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "/ui?q=ok" {
		t.Fatalf("forwarded path %q: %v", body, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if len(operatorBrowserSlots) != before {
		t.Fatal("connection close did not join its local viewer")
	}
}

func TestOperatorMeshAttachedHTTPRunOpensOwnedLocalBrowserURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("running mesh app")) }))
	defer server.Close()
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	device, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	conn := &grpcclient.AgentConnection{OperatorMeshTarget: device, Reconnect: neverReconnect, AppDialer: func(ctx context.Context, port uint16) (net.Conn, error) {
		if port != 8080 {
			t.Errorf("selected app port=%d", port)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previousOpen, previousAllowed := browserOpen, postStartBrowserAllowed
	t.Cleanup(func() { browserOpen, postStartBrowserAllowed = previousOpen, previousAllowed })
	opened := ""
	browserOpen = func(address string) error { opened = address; return nil }
	postStartBrowserAllowed = func() bool { return true }
	cfg := &appconfig.AppConfig{AppID: "running-http", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}}
	if child := runPostStartIfReady(ctx, ctx, conn, cfg, runOptions{}); child != nil {
		t.Fatal("HTTP-only app unexpectedly spawned a CLI hook")
	}
	if !strings.HasPrefix(opened, "http://127.0.0.1:") {
		t.Fatalf("attached app browser URL=%s", opened)
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	response, err := client.Get(opened)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(content) != "running mesh app" {
		t.Fatalf("attached app response %q: %v", content, err)
	}
}
