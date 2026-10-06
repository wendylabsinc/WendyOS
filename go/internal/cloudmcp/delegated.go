package cloudmcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/delegation"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type userDelegatedKey struct {
	mu           sync.Mutex
	privatePEM   string
	owner, id    string
	csr, binding string
	expires      time.Time
	cached       *issuedDelegation
}

// ConfigureDelegations pins operator/device roots independently of Cloud's
// issuance response. Call once before serving requests. Keys remain in memory;
// restarting the gateway requires a new signed user approval.
func (b *CloudBackend) ConfigureDelegations(rootPEM []byte, relayIssuer, origin string) error {
	roots := x509.NewCertPool()
	u, err := url.Parse(relayIssuer)
	if !roots.AppendCertsFromPEM(rootPEM) || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("delegation roots and HTTPS relay issuer are required")
	}
	o, err := url.Parse(origin)
	if err != nil || o.Scheme != "https" || o.Host == "" || o.User != nil || o.RawQuery != "" || o.Fragment != "" || o.Path != "" {
		return fmt.Errorf("HTTPS MCP origin without path is required")
	}
	b.mcpOrigin = origin
	b.roots = roots
	b.rootsPEM = string(rootPEM)
	b.relayIssuer = relayIssuer
	b.keys = map[string]*userDelegatedKey{}
	return nil
}

func delegatedCacheKey(a Access) string {
	return a.TenantID + "\x00" + a.UserID + "\x00" + a.ServiceSubject
}

func (b *CloudBackend) prepareKey(ctx context.Context, a Access) (*userDelegatedKey, error) {
	b.mu.Lock()
	for id, key := range b.keys {
		if time.Now().After(key.expires) {
			delete(b.keys, id)
		}
	}
	entry := b.keys[delegatedCacheKey(a)]
	if entry == nil {
		if len(b.keys) >= 256 {
			b.mu.Unlock()
			return nil, fmt.Errorf("delegation capacity reached")
		}
		key, err := certs.GenerateMLDSAKeyPair()
		if err != nil {
			b.mu.Unlock()
			return nil, err
		}
		entry = &userDelegatedKey{privatePEM: key, owner: a.OwnerPrincipal, expires: time.Now().Add(24 * time.Hour)}
		b.keys[delegatedCacheKey(a)] = entry
	}
	b.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.owner != a.OwnerPrincipal {
		return nil, errDenied
	}
	if entry.csr == "" {
		csrPEM, err := certs.GenerateCSR([]byte(entry.privatePEM), "hosted-mcp", []string{entry.owner}, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode([]byte(csrPEM))
		if block == nil {
			return nil, fmt.Errorf("invalid generated CSR")
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
		entry.csr = base64.StdEncoding.EncodeToString(block.Bytes)
		entry.binding = base64.RawURLEncoding.EncodeToString(digest[:])
	}
	raw, err := b.request(ctx, a.OrganizationID, "credential", map[string]string{"user_token": a.bearer, "csr": entry.csr, "key_binding": entry.binding})
	if err != nil {
		return nil, err
	}
	var pending struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &pending) != nil || !canonicalUUID(pending.ID) {
		return nil, fmt.Errorf("invalid pending delegation")
	}
	if entry.id != pending.ID {
		entry.cached = nil
	}
	entry.id = pending.ID
	return entry, nil
}

func (b *CloudBackend) authorizeDelegated(ctx context.Context, token, org, device, method string) (Access, error) {
	identity, err := b.authorize(ctx, token, org, "", "mcp.connect", "")
	if err != nil || !identity.permits(org) {
		return identity, err
	}
	entry, err := b.prepareKey(ctx, identity)
	if err != nil {
		return Access{}, err
	}
	if method == "mcp.connect" {
		return identity, nil
	}
	entry.mu.Lock()
	id := entry.id
	entry.mu.Unlock()
	return b.authorize(ctx, token, org, device, method, id)
}

type issuedDelegation struct {
	ID            string            `json:"id"`
	Pending       bool              `json:"pending"`
	Certificate   []byte            `json:"certificate"`
	Chain         []byte            `json:"chain"`
	Devices       map[string]string `json:"devices"`
	Specification struct {
		Delegation struct {
			Version  int      `json:"version"`
			ID       string   `json:"id"`
			Owner    string   `json:"owner_principal"`
			Devices  []string `json:"device_principals"`
			Apps     []string `json:"app_ids"`
			Audience string   `json:"audience"`
			Gateway  string   `json:"gateway_principal"`
		} `json:"delegation"`
		Entitlements []string `json:"entitlements"`
		Exp          int64    `json:"exp"`
	} `json:"specification"`
}

func (b *CloudBackend) inspectIssued(a Access, entry *userDelegatedKey, issued issuedDelegation) (*x509.Certificate, *delegation.Scope, string, error) {
	leaf, err := x509.ParseCertificate(issued.Certificate)
	if err != nil {
		return nil, nil, "", err
	}
	scope, err := delegation.Parse(leaf)
	if err != nil || scope == nil {
		return nil, nil, "", fmt.Errorf("missing delegated constraints")
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(entry.privatePEM))
	if err != nil {
		return nil, nil, "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil || !bytes.Equal(pub, leaf.RawSubjectPublicKeyInfo) {
		return nil, nil, "", fmt.Errorf("delegated key mismatch")
	}
	want := issued.Specification.Delegation
	gateway := "spiffe://wendy.sh/tenant/" + a.TenantID + "/service/" + a.ServiceSubject
	if scope.Version != 2 || scope.ID != entry.id || scope.ID != a.DelegationID || scope.Owner != entry.owner || scope.Owner != want.Owner || scope.ID != want.ID || scope.Gateway != gateway || scope.Gateway != want.Gateway || scope.Audience != b.mcpOrigin+"/orgs/"+a.OrganizationID+"/mcp" || scope.Audience != want.Audience || !reflect.DeepEqual(scope.Devices, want.Devices) || !reflect.DeepEqual(scope.Apps, want.Apps) || !sameStrings(scope.Rules(), issued.Specification.Entitlements) || leaf.NotAfter.After(time.Now().Add(5*time.Minute)) || leaf.NotAfter.Unix() > issued.Specification.Exp {
		return nil, nil, "", fmt.Errorf("issued certificate exceeds approved delegation")
	}
	intermediates := x509.NewCertPool()
	chainPEM := ""
	chain, err := x509.ParseCertificates(issued.Chain)
	if err != nil {
		return nil, nil, "", err
	}
	for _, c := range chain {
		intermediates.AddCert(c)
		chainPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	checked := *leaf
	checked.UnhandledCriticalExtensions = []asn1.ObjectIdentifier{}
	for _, oid := range leaf.UnhandledCriticalExtensions {
		if !oid.Equal(delegation.ScopeOID) {
			checked.UnhandledCriticalExtensions = append(checked.UnhandledCriticalExtensions, oid)
		}
	}
	verifiedChains, err := checked.Verify(x509.VerifyOptions{Roots: b.roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return nil, nil, "", err
	}
	// Retain only issuers from the root-anchored verified path, never arbitrary
	// additional certificates supplied by the Cloud response.
	chainPEM = b.rootsPEM
	for _, issuer := range verifiedChains[0][1:] {
		chainPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw}))
	}
	return leaf, scope, chainPEM, nil
}

func (b *CloudBackend) connectDelegated(ctx context.Context, a Access, device string) (*grpcclient.AgentConnection, error) {
	if b.roots == nil || !a.permits(a.OrganizationID) || a.bearer == "" || a.DelegationID == "" {
		return nil, errUserAuthorityRequired
	}
	b.mu.Lock()
	entry := b.keys[delegatedCacheKey(a)]
	b.mu.Unlock()
	if entry == nil {
		return nil, errUserAuthorityRequired
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.id != a.DelegationID {
		return nil, errUserAuthorityRequired
	}
	var issued issuedDelegation
	if entry.cached != nil {
		leaf, err := x509.ParseCertificate(entry.cached.Certificate)
		if err == nil && leaf.NotAfter.After(time.Now().Add(30*time.Second)) {
			issued = *entry.cached
		}
	}
	if len(issued.Certificate) == 0 {
		raw, err := b.request(ctx, a.OrganizationID, "credential", map[string]string{"id": entry.id, "user_token": a.bearer})
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &issued) != nil || issued.Pending {
			return nil, errUserAuthorityRequired
		}
	}
	leaf, scope, chainPEM, err := b.inspectIssued(a, entry, issued)
	if err != nil {
		return nil, err
	}
	entry.cached = &issued
	principal := issued.Devices[device]
	if principal == "" {
		return nil, errDenied
	}
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
	sign, err := cloudrelay.PrincipalSigner(leafPEM, []byte(entry.privatePEM))
	if err != nil {
		return nil, err
	}
	delegationID := entry.id
	request := func(c context.Context, r *pb.RequestTunnelRequest) (*pb.RequestTunnelResponse, error) {
		encoded, err := proto.Marshal(r)
		if err != nil {
			return nil, err
		}
		data, err := b.request(c, a.OrganizationID, "tunnel", map[string]string{"id": delegationID, "user_token": a.bearer, "tunnel_request": base64.StdEncoding.EncodeToString(encoded)})
		if err != nil {
			return nil, err
		}
		var response struct {
			Response []byte `json:"response"`
		}
		if json.Unmarshal(data, &response) != nil {
			return nil, fmt.Errorf("invalid tunnel response")
		}
		result := new(pb.RequestTunnelResponse)
		if err := proto.Unmarshal(response.Response, result); err != nil {
			return nil, err
		}
		return result, nil
	}
	verifier := &cloudrelay.Verifier{Issuer: b.relayIssuer, RelayDial: func(endpoint string) (*grpc.ClientConn, error) {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, err
		}
		return grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) {
			return (&tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}}).DialContext(c, "tcp", target)
		}))
	}}
	binding := map[string]string{"delegation_id": scope.ID, "device_principal": principal, "audience": scope.Audience, "gateway_principal": scope.Gateway}
	expected, err := certs.ParsePrincipal(principal)
	if err != nil {
		return nil, err
	}
	info := &config.CertificateInfo{PemCertificate: leafPEM, PemCertificateChain: chainPEM, PemPrivateKey: entry.privatePEM}
	return grpcclient.ConnectWithTLSExpecting(ctx, "passthrough:///delegated-agent", info, nil, &expected, clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) {
		return cloudrelay.OpenTCPWithRequester(c, ctx, request, verifier, device, "wendy-agent", sign, binding, leaf.NotAfter)
	}))

}

func sameStrings(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
