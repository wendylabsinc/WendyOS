package cloudmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/propagation"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

var ErrUnauthenticated = errors.New("MCP token rejected")
var errDenied = errors.New("MCP operation denied")

// CloudBackend delegates live policy to Cloud over TLS with the organization's
// DPoP-bound machine credential. Sessions are configured by the service operator;
// no client request can introduce a key, issuer, or destination.
type CloudBackend struct {
	origin   string
	sessions map[string]*browserauth.MachineSession
	client   *http.Client
}

func NewCloudBackend(origin string, sessions map[string]*browserauth.MachineSession) (*CloudBackend, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("authorization backend must be an HTTPS origin")
	}
	copy := make(map[string]*browserauth.MachineSession, len(sessions))
	for org, session := range sessions {
		if !canonicalUUID(org) || session == nil {
			return nil, fmt.Errorf("invalid configured machine session")
		}
		copy[org] = session
	}
	return &CloudBackend{origin: origin, sessions: copy, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (b *CloudBackend) request(ctx context.Context, org, operation string, body any) ([]byte, error) {
	if !canonicalUUID(org) {
		return nil, fmt.Errorf("invalid organization")
	}
	endpoint := b.origin + "/v1/hosted-mcp/orgs/" + org + "/" + operation
	method := http.MethodGet
	var reader io.Reader
	if body != nil {
		method = http.MethodPost
		encoded, err := json.Marshal(body)
		if err != nil || len(encoded) > 65536 {
			return nil, fmt.Errorf("invalid authorization request")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		machine := b.sessions[org]
		if machine == nil {
			return nil, fmt.Errorf("organization machine is not provisioned")
		}
		auth, err := machine.Credentials(ctx)
		if err != nil {
			return nil, err
		}
		key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.DPoPPrivateKey))
		if err != nil {
			return nil, err
		}
		proof, err := cloudrequest.NewDPoPAccessProof(key, method, endpoint, auth.APIKey)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "DPoP "+auth.APIKey)
		req.Header.Set("DPoP", proof)
		req.Header.Set("Content-Type", "application/json")
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	if id, ok := ctx.Value(correlationKey{}).(string); ok {
		req.Header.Set("X-Correlation-ID", id)
	}
	response, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorization backend unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthenticated
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, errDenied
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authorization backend unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		return nil, fmt.Errorf("invalid authorization response")
	}
	return data, nil
}

func (b *CloudBackend) Organization(ctx context.Context, org string) (Organization, error) {
	data, err := b.request(ctx, org, "discovery", nil)
	if err != nil {
		return Organization{}, err
	}
	var value Organization
	err = json.Unmarshal(data, &value)
	return value, err
}
func (b *CloudBackend) Authorize(ctx context.Context, token, org, device, method string) (Access, error) {
	data, err := b.request(ctx, org, "authorize", map[string]string{"user_token": token, "device": device, "method": method})
	if errors.Is(err, errDenied) {
		return Access{}, nil
	}
	if err != nil {
		return Access{}, err
	}
	var access Access
	if err := json.Unmarshal(data, &access); err != nil {
		return Access{}, fmt.Errorf("invalid authorization response")
	}
	if !access.permits(org) {
		return Access{}, nil
	}
	access.bearer = token
	return access, nil
}
func (b *CloudBackend) Devices(ctx context.Context, access Access) (json.RawMessage, error) {
	if !access.permits(access.OrganizationID) || access.bearer == "" {
		return nil, fmt.Errorf("inventory access denied")
	}
	return b.request(ctx, access.OrganizationID, "devices", map[string]string{"user_token": access.bearer})
}
func (b *CloudBackend) Connect(ctx context.Context, access Access, device string) (*grpcclient.AgentConnection, error) {
	session := b.sessions[access.OrganizationID]
	if session == nil || !access.permits(access.OrganizationID) {
		return nil, fmt.Errorf("device access denied")
	}
	return session.ConnectDevice(ctx, access.TenantID, access.ServiceSubject, device)
}
