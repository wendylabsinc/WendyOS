package cloudmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

const testOrg = "11111111-1111-4111-8111-111111111111"
const testTenant = "22222222-2222-4222-8222-222222222222"
const testDevice = "33333333-3333-4333-8333-333333333333"

type fakeBackend struct {
	access     Access
	err        error
	denyMethod string
	calls      []string
}

func (b *fakeBackend) Organization(context.Context, string) (Organization, error) {
	return Organization{Issuer: "https://auth.example/realms/example"}, nil
}
func (b *fakeBackend) Authorize(_ context.Context, token, org, device, method string) (Access, error) {
	if token != "user-token" {
		return Access{}, ErrUnauthenticated
	}
	b.calls = append(b.calls, method)
	a := b.access
	if method == b.denyMethod {
		a.UserAllowed = false
	}
	return a, b.err
}
func (b *fakeBackend) Devices(context.Context, Access) (json.RawMessage, error) {
	return json.RawMessage(`[{"id":"` + testDevice + `"}]`), nil
}

type fakeConnector struct{ calls int }

func (c *fakeConnector) Connect(context.Context, Access, string) (*grpcclient.AgentConnection, error) {
	c.calls++
	return nil, errors.New("offline")
}

func fixture(t *testing.T) (*Server, *fakeBackend, *fakeConnector) {
	t.Helper()
	b := &fakeBackend{access: Access{OrganizationID: testOrg, TenantID: testTenant, UserID: "user", ServiceSubject: "machine", Enabled: true, UserAllowed: true, ServiceAllowed: true, ExpiresAt: time.Now().Add(time.Hour)}}
	c := &fakeConnector{}
	s, err := New(ProductionURL, b, c)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, c
}
func request(s *Server, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ProductionURL+"/orgs/"+testOrg+"/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestDiscoveryUsesCanonicalOrganizationResource(t *testing.T) {
	s, _, _ := fixture(t)
	r := httptest.NewRequest(http.MethodGet, ProductionURL+"/.well-known/oauth-protected-resource/orgs/"+testOrg+"/mcp", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var metadata map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["resource"] != ProductionURL+"/orgs/"+testOrg+"/mcp" {
		t.Fatal(metadata)
	}
	w = request(s, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/orgs/"+testOrg+"/mcp") {
		t.Fatal(w.Code, w.Header())
	}
}

func TestAccessRequiresBothPrincipalsAndLiveOrganization(t *testing.T) {
	for _, scenario := range []string{"user", "service", "disabled", "expired", "wrong-org", "missing-tenant", "missing-user", "missing-service", "backend-error"} {
		t.Run(scenario, func(t *testing.T) {
			s, b, c := fixture(t)
			switch scenario {
			case "user":
				b.access.UserAllowed = false
			case "service":
				b.access.ServiceAllowed = false
			case "disabled":
				b.access.Enabled = false
			case "expired":
				b.access.ExpiresAt = time.Now().Add(-time.Second)
			case "wrong-org":
				b.access.OrganizationID = testTenant
			case "missing-tenant":
				b.access.TenantID = ""
			case "missing-user":
				b.access.UserID = ""
			case "missing-service":
				b.access.ServiceSubject = ""
			case "backend-error":
				b.err = errors.New("database unavailable")
			}
			w := request(s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Bearer user-token")
			if (scenario == "backend-error" && w.Code != http.StatusServiceUnavailable) || (scenario != "backend-error" && w.Code != http.StatusForbidden) {
				t.Fatal(w.Code, w.Body.String())
			}
			if c.calls != 0 {
				t.Fatal("denied caller reached device")
			}
		})
	}
}

func TestMCPDeviceInventoryAndFreshToolAuthorization(t *testing.T) {
	s, b, c := fixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_list","arguments":{}}}`
	w := request(s, body, "Bearer user-token")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), testDevice) {
		t.Fatal(w.Code, w.Body.String())
	}
	b.denyMethod = "device_list"
	w = request(s, body, "Bearer user-token")
	if !strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), testDevice) {
		t.Fatal(w.Code, w.Body.String())
	}
	if c.calls != 0 {
		t.Fatal("inventory dialed a device")
	}
}

func TestHostAndOriginCannotChangeResource(t *testing.T) {
	for _, scenario := range []string{"host", "origin", "encoded-org", "trailing-slash"} {
		t.Run(scenario, func(t *testing.T) {
			s, b, _ := fixture(t)
			path := "/orgs/" + testOrg + "/mcp"
			if scenario == "encoded-org" {
				path = strings.Replace(path, "1", "%31", 1)
			}
			if scenario == "trailing-slash" {
				path += "/"
			}
			r := httptest.NewRequest(http.MethodPost, ProductionURL+path, nil)
			r.Header.Set("Authorization", "Bearer user-token")
			if scenario == "host" {
				r.Host = "attacker.example"
			}
			if scenario == "origin" {
				r.Header.Set("Origin", "https://attacker.example")
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code < 400 || len(b.calls) != 0 {
				t.Fatal(w.Code, b.calls)
			}
		})
	}
}

func TestAgentCatalogRejectsCloudAndUnknownMethods(t *testing.T) {
	s, _, _ := fixture(t)
	w := request(s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_methods","arguments":{}}}`, "Bearer user-token")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "GetAgentVersion") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"/wendycloud.v2.OrganizationService/DeleteOrganization", "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", "/missing/Unknown", "../../admin"} {
		if _, err := agentMethod(method); err == nil {
			t.Fatalf("accepted %s", method)
		}
	}
}

func (*fakeBackend) Record(context.Context, Access, GatewayEvent) error { return nil }
