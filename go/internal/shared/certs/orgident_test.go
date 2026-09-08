package certs

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"testing"
)

// mustParseURL is a test helper that parses a URL or fatals.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestScopeFromCert(t *testing.T) {
	tests := []struct {
		name       string
		cn         string
		uris       []string
		wantOrg    int32
		wantHasOrg bool
		wantErr    bool
	}{
		// SAN URI: user type.
		{
			name:       "SAN user URN",
			uris:       []string{"urn:wendy:org:7:user:abc"},
			wantOrg:    7,
			wantHasOrg: true,
		},
		// SAN URI: asset type.
		{
			name:       "SAN asset URN",
			uris:       []string{"urn:wendy:org:42:asset:5"},
			wantOrg:    42,
			wantHasOrg: true,
		},
		// Non-wendy URI before valid wendy URN — the wendy URN is still found.
		{
			name:       "non-wendy URI before wendy URN",
			uris:       []string{"spiffe://x", "urn:wendy:org:99:asset:1"},
			wantOrg:    99,
			wantHasOrg: true,
		},
		// No URIs, device CN.
		{
			name:       "no URIs device CN sh/wendy/7/5",
			cn:         "sh/wendy/7/5",
			wantOrg:    7,
			wantHasOrg: true,
		},
		// No URIs, user CN — no org (not an error).
		{
			name:       "no URIs user CN wendy/user/abc",
			cn:         "wendy/user/abc",
			wantOrg:    0,
			wantHasOrg: false,
		},
		// No URIs, empty CN.
		{
			name:       "no URIs empty CN",
			cn:         "",
			wantOrg:    0,
			wantHasOrg: false,
		},
		// No URIs, unrelated CN.
		{
			name:       "no URIs unrelated CN",
			cn:         "example.com",
			wantOrg:    0,
			wantHasOrg: false,
		},
		// Malformed wendy URN: non-numeric org.
		{
			name:    "malformed URN non-numeric org",
			uris:    []string{"urn:wendy:org:notanumber:user:x"},
			wantErr: true,
		},
		// Malformed wendy URN: wrong segment count (5 parts).
		{
			name:    "malformed URN wrong segment count",
			uris:    []string{"urn:wendy:org:7:user"},
			wantErr: true,
		},
		// Unknown entity type.
		{
			name:    "URN unknown entity type",
			uris:    []string{"urn:wendy:org:7:group:x"},
			wantErr: true,
		},
		// Valid wendy URN present — CN with different sh/wendy value must NOT be consulted.
		{
			name:       "SAN wins over CN",
			cn:         "sh/wendy/100/200",
			uris:       []string{"urn:wendy:org:7:asset:1"},
			wantOrg:    7,
			wantHasOrg: true,
		},
		// No URIs, CN sh/wendy/abc/5 — bad org in CN.
		{
			name:    "CN sh/wendy bad org",
			cn:      "sh/wendy/abc/5",
			wantErr: true,
		},
		// Two wendy URNs — ambiguous, must error.
		{
			name:    "two wendy URNs ambiguous",
			uris:    []string{"urn:wendy:org:10:user:a", "urn:wendy:org:20:user:b"},
			wantErr: true,
		},
		// URN with org ID 0 — non-positive, must error.
		{
			name:    "URN org zero",
			uris:    []string{"urn:wendy:org:0:user:abc"},
			wantErr: true,
		},
		// URN with negative org ID — non-positive, must error.
		{
			name:    "URN org negative",
			uris:    []string{"urn:wendy:org:-1:user:abc"},
			wantErr: true,
		},
		// URN with int32 overflow org ID — must error.
		{
			name:    "URN org int32 overflow",
			uris:    []string{"urn:wendy:org:2147483648:user:abc"},
			wantErr: true,
		},
		// URN with empty org segment — must error.
		{
			name:    "URN empty org segment",
			uris:    []string{"urn:wendy:org::user:abc"},
			wantErr: true,
		},
		// CN with org ID 0 — non-positive, must error.
		{
			name:    "CN sh/wendy org zero",
			cn:      "sh/wendy/0/5",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cert := &x509.Certificate{
				Subject: pkix.Name{CommonName: tc.cn},
			}
			for _, raw := range tc.uris {
				cert.URIs = append(cert.URIs, mustParseURL(t, raw))
			}

			scope, hasOrg, err := ScopeFromCert(cert)

			if tc.wantErr {
				if err == nil {
					t.Errorf("ScopeFromCert() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ScopeFromCert() unexpected error: %v", err)
			}
			if scope.OrgID != tc.wantOrg {
				t.Errorf("ScopeFromCert() orgID = %d, want %d", scope.OrgID, tc.wantOrg)
			}
			if hasOrg != tc.wantHasOrg {
				t.Errorf("ScopeFromCert() known = %v, want %v", hasOrg, tc.wantHasOrg)
			}
		})
	}
}

func TestIdentityFromCert(t *testing.T) {
	mustParseSANURI := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing URI %q: %v", raw, err)
		}
		return u
	}
	makeCert := func(cn string, uris ...string) *x509.Certificate {
		c := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		for _, u := range uris {
			c.URIs = append(c.URIs, mustParseSANURI(u))
		}
		return c
	}

	tests := []struct {
		name    string
		cert    *x509.Certificate
		wantID  WendyIdentity
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "SAN URI asset",
			cert:   makeCert("ignored", "urn:wendy:org:7:asset:42"),
			wantID: WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"},
			wantOK: true,
		},
		{
			name:   "SAN URI user",
			cert:   makeCert("ignored", "urn:wendy:org:3:user:99"),
			wantID: WendyIdentity{OrgID: 3, EntityType: "user", EntityID: "99"},
			wantOK: true,
		},
		{
			name:   "CN fallback sh/wendy",
			cert:   makeCert("sh/wendy/5/123"),
			wantID: WendyIdentity{OrgID: 5, EntityType: "asset", EntityID: "123"},
			wantOK: true,
		},
		{
			name:   "no identity",
			cert:   makeCert("wendy/user/99"),
			wantOK: false,
		},
		{
			name:    "multiple wendy URNs",
			cert:    makeCert("", "urn:wendy:org:1:asset:1", "urn:wendy:org:2:asset:2"),
			wantErr: true,
		},
		{
			name:    "malformed URN",
			cert:    makeCert("", "urn:wendy:org:0:asset:5"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok, err := IdentityFromCert(tt.cert)
			if (err != nil) != tt.wantErr {
				t.Fatalf("IdentityFromCert() error = %v, wantErr %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && id != tt.wantID {
				t.Errorf("identity = %+v, want %+v", id, tt.wantID)
			}
		})
	}
}

func TestScopeFromCert_LegacyURN(t *testing.T) {
	mustParseURI := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return u
	}
	cert := &x509.Certificate{
		URIs: []*url.URL{mustParseURI("urn:wendy:org:7:asset:42")},
	}
	scope, ok, err := ScopeFromCert(cert)
	if err != nil || !ok || scope != (Scope{OrgID: 7}) {
		t.Errorf("ScopeFromCert() = %+v, %v, %v; want {OrgID:7}, true, nil", scope, ok, err)
	}
}
