package interceptor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// mustParseURL parses a raw URL for use in a cert's SAN URIs, failing the test
// if it cannot be parsed.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing URL %q: %v", raw, err)
	}
	return u
}

// leafOptions configures the hand-constructed leaf certificate used in tests.
type leafOptions struct {
	commonName  string
	uris        []*url.URL
	noClientEKU bool
}

// buildLeaf constructs an *x509.Certificate suitable for the CheckMTLS struct-field
// inspection. CheckMTLS only reads struct fields (the handshake already verified the
// signature) so a literal works — no real signed cert is needed.
func buildLeaf(opts leafOptions) *x509.Certificate {
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: opts.commonName},
		URIs:         opts.uris,
	}
	if !opts.noClientEKU {
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	return leaf
}

// ctxWithLeaf builds a gRPC context carrying the given leaf as the peer's single
// presented client certificate.
func ctxWithLeaf(leaf *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
			},
		},
	})
}

func TestCheckMTLS_OrgEnforcement(t *testing.T) {
	logger := zap.NewNop()
	expectedScope := certs.Scope{OrgID: 7}

	tests := []struct {
		name     string
		leaf     *x509.Certificate
		mode     OrgMode
		wantCode codes.Code // codes.OK means no error expected
	}{
		{
			name:     "asset CN org match grace",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/7/5"}),
			mode:     OrgModeGrace,
			wantCode: codes.OK,
		},
		{
			name:     "asset CN org match strict",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/7/5"}),
			mode:     OrgModeStrict,
			wantCode: codes.OK,
		},
		{
			name:     "user SAN org match grace",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:7:user:abc")}}),
			mode:     OrgModeGrace,
			wantCode: codes.OK,
		},
		{
			name:     "user SAN org match strict",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:7:user:abc")}}),
			mode:     OrgModeStrict,
			wantCode: codes.OK,
		},
		{
			name:     "CN org mismatch grace",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/9/5"}),
			mode:     OrgModeGrace,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "CN org mismatch strict",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/9/5"}),
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "user SAN mismatch grace",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:9:user:x")}}),
			mode:     OrgModeGrace,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "user SAN mismatch strict",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:9:user:x")}}),
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "no-org legacy user cert grace allowed",
			leaf:     buildLeaf(leafOptions{commonName: "wendy/user/abc"}),
			mode:     OrgModeGrace,
			wantCode: codes.OK,
		},
		{
			name:     "no-org legacy user cert strict rejected",
			leaf:     buildLeaf(leafOptions{commonName: "wendy/user/abc"}),
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "malformed org claim grace rejected",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:0:user:x")}}),
			mode:     OrgModeGrace,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "malformed org claim strict rejected",
			leaf:     buildLeaf(leafOptions{uris: []*url.URL{mustParseURL(t, "urn:wendy:org:0:user:x")}}),
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "mode off skips org check on mismatch",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/9/5"}),
			mode:     OrgModeOff,
			wantCode: codes.OK,
		},
		{
			name:     "regression no clientAuth EKU rejected in grace",
			leaf:     buildLeaf(leafOptions{commonName: "sh/wendy/7/5", noClientEKU: true}),
			mode:     OrgModeGrace,
			wantCode: codes.Unauthenticated,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxWithLeaf(tc.leaf)
			err := CheckMTLS(ctx, logger, expectedScope, tc.mode)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("CheckMTLS code = %v (err=%v); want %v", got, err, tc.wantCode)
			}
		})
	}
}

func TestCheckMTLS_NoPeerCertificates(t *testing.T) {
	logger := zap.NewNop()
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: nil}},
	})
	err := CheckMTLS(ctx, logger, certs.Scope{OrgID: 7}, OrgModeGrace)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("CheckMTLS code = %v (err=%v); want Unauthenticated", got, err)
	}
}

func TestParseOrgMode(t *testing.T) {
	tests := []struct {
		in       string
		wantMode OrgMode
		wantOK   bool
	}{
		{"", OrgModeStrict, true},
		{"grace", OrgModeGrace, true},
		{"GRACE", OrgModeGrace, true},
		{" strict ", OrgModeStrict, true},
		{"strict", OrgModeStrict, true},
		{"off", OrgModeOff, true},
		{"OFF", OrgModeOff, true},
		{"bogus", OrgModeStrict, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			gotMode, gotOK := ParseOrgMode(tc.in)
			if gotMode != tc.wantMode || gotOK != tc.wantOK {
				t.Fatalf("ParseOrgMode(%q) = (%v, %v); want (%v, %v)", tc.in, gotMode, gotOK, tc.wantMode, tc.wantOK)
			}
		})
	}
}

func TestOrgModeString(t *testing.T) {
	tests := []struct {
		mode OrgMode
		want string
	}{
		{OrgModeOff, "off"},
		{OrgModeGrace, "grace"},
		{OrgModeStrict, "strict"},
	}
	for _, tc := range tests {
		if got := tc.mode.String(); got != tc.want {
			t.Fatalf("OrgMode(%d).String() = %q; want %q", tc.mode, got, tc.want)
		}
	}
}

const testTenant = "6f1b7d3c-6b7e-4a2f-9c1e-2b4a8d5e0f31"
const otherTenant = "00000000-0000-4000-8000-000000000000"

// TestCheckMTLS_TenantEnforcement covers what WDY-2968 changed: a peer whose
// only identity is a tenant SPIFFE principal is a recognised caller, compared
// against this device's own tenant. The three-way split matters — a tenant that
// differs is refused under every mode, while a tenant that simply cannot be
// compared with this device's is the rotation window grace exists for.
func TestCheckMTLS_TenantEnforcement(t *testing.T) {
	logger := zap.NewNop()
	deviceTenant := certs.Scope{TenantUUID: testTenant}
	deviceOrg := certs.Scope{OrgID: 7}

	principal := func(t *testing.T, tenant, rest string) *x509.Certificate {
		return buildLeaf(leafOptions{
			uris: []*url.URL{mustParseURL(t, "spiffe://wendy.sh/tenant/"+tenant+"/"+rest)},
		})
	}

	tests := []struct {
		name     string
		leaf     *x509.Certificate
		expected certs.Scope
		mode     OrgMode
		wantCode codes.Code // codes.OK means "allowed"
	}{
		{
			// The bug: before this, an operator leaf renewed by pki-core had no
			// urn:wendy SAN, so strict answered PermissionDenied on every RPC.
			name:     "same tenant, operator principal, strict",
			leaf:     principal(t, testTenant, "operator/auth0|abc"),
			expected: deviceTenant,
			mode:     OrgModeStrict,
			wantCode: codes.OK,
		},
		{
			name:     "same tenant, cloud-relayed user principal, strict",
			leaf:     principal(t, testTenant, "service/user-5"),
			expected: deviceTenant,
			mode:     OrgModeStrict,
			wantCode: codes.OK,
		},
		{
			name:     "different tenant is refused under strict",
			leaf:     principal(t, otherTenant, "operator/auth0|abc"),
			expected: deviceTenant,
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			// Grace forgives an identity it cannot read, never one it can read
			// and that says someone else.
			name:     "different tenant is refused under grace too",
			leaf:     principal(t, otherTenant, "operator/auth0|abc"),
			expected: deviceTenant,
			mode:     OrgModeGrace,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "incomparable scopes are refused under strict",
			leaf:     principal(t, testTenant, "operator/auth0|abc"),
			expected: deviceOrg,
			mode:     OrgModeStrict,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "incomparable scopes are allowed under grace",
			leaf:     principal(t, testTenant, "operator/auth0|abc"),
			expected: deviceOrg,
			mode:     OrgModeGrace,
			wantCode: codes.OK,
		},
		{
			// A transitional leaf carries both SANs, so an old-chain device can
			// still compare the org it understands.
			name: "transitional leaf matches an org-only device",
			leaf: buildLeaf(leafOptions{uris: []*url.URL{
				mustParseURL(t, "spiffe://wendy.sh/tenant/"+testTenant+"/service/asset-42"),
				mustParseURL(t, "urn:wendy:org:7:asset:42"),
			}}),
			expected: deviceOrg,
			mode:     OrgModeStrict,
			wantCode: codes.OK,
		},
		{
			name:     "off skips the check entirely",
			leaf:     principal(t, otherTenant, "operator/auth0|abc"),
			expected: deviceTenant,
			mode:     OrgModeOff,
			wantCode: codes.OK,
		},
		{
			name:     "a malformed principal is anomalous, refused under grace",
			leaf:     principal(t, "not-a-uuid", "operator/auth0|abc"),
			expected: deviceTenant,
			mode:     OrgModeGrace,
			wantCode: codes.PermissionDenied,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMTLS(ctxWithLeaf(tc.leaf), logger, tc.expected, tc.mode)
			if tc.wantCode == codes.OK {
				if err != nil {
					t.Fatalf("CheckMTLS = %v, want allowed", err)
				}
				return
			}
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("CheckMTLS code = %v (%v), want %v", got, err, tc.wantCode)
			}
		})
	}
}
