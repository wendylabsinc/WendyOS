package certs

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"strings"
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

func TestOrgFromClientCert(t *testing.T) {
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

			org, hasOrg, err := OrgFromClientCert(cert)

			if tc.wantErr {
				if err == nil {
					t.Errorf("OrgFromClientCert() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("OrgFromClientCert() unexpected error: %v", err)
			}
			if org != tc.wantOrg {
				t.Errorf("OrgFromClientCert() orgID = %d, want %d", org, tc.wantOrg)
			}
			if hasOrg != tc.wantHasOrg {
				t.Errorf("OrgFromClientCert() hasOrg = %v, want %v", hasOrg, tc.wantHasOrg)
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

func TestOrgFromClientCert_StillWorks(t *testing.T) {
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
	orgID, ok, err := OrgFromClientCert(cert)
	if err != nil || !ok || orgID != 7 {
		t.Errorf("OrgFromClientCert() = %d, %v, %v; want 7, true, nil", orgID, ok, err)
	}
}

func TestDeviceSPIFFEURI(t *testing.T) {
	const tenant = "022b7284-f7f3-4d86-b844-d105a7c06d9e"
	// A multi-segment device name is first-class: pki-core's own convention
	// for a Wendy device is "sh/wendy/<org>/<asset>".
	got := DeviceSPIFFEURI(tenant, "sh/wendy/2/408")
	want := "spiffe://wendy.sh/tenant/" + tenant + "/device/sh/wendy/2/408"
	if got != want {
		t.Errorf("DeviceSPIFFEURI = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, TenantSPIFFEPrefix) {
		t.Errorf("%q does not start with the tenant prefix", got)
	}
}

func TestParseDeviceSPIFFEURI(t *testing.T) {
	const tenant = "022b7284-f7f3-4d86-b844-d105a7c06d9e"

	tenantGot, name, err := ParseDeviceSPIFFEURI(DeviceSPIFFEURI(tenant, "sh/wendy/2/408"))
	if err != nil {
		t.Fatalf("ParseDeviceSPIFFEURI: %v", err)
	}
	if tenantGot != tenant {
		t.Errorf("tenant = %q, want %q", tenantGot, tenant)
	}
	if name != "sh/wendy/2/408" {
		t.Errorf("name = %q, want the whole multi-segment name", name)
	}

	// A service or operator principal must be an error and not a miss:
	// accepting one where a device identity is required is the mistake this
	// parser exists to prevent, and the data platform's ingest interceptor
	// rejects any kind but "device" anyway.
	for _, bad := range []string{
		"spiffe://wendy.sh/tenant/" + tenant + "/service/asset-408",
		"spiffe://wendy.sh/tenant/" + tenant + "/operator/martien",
		"urn:wendy:org:2:asset:408",
		"spiffe://wendy.sh/tenant/" + tenant,
		"spiffe://wendy.sh/tenant/" + tenant + "/device/",
		"spiffe://wendy.sh/tenant//device/x",
	} {
		if _, _, err := ParseDeviceSPIFFEURI(bad); err == nil {
			t.Errorf("ParseDeviceSPIFFEURI(%q) = nil error, want a refusal", bad)
		}
	}
}

func TestTenantSPIFFEURIs(t *testing.T) {
	const tenant = "022b7284-f7f3-4d86-b844-d105a7c06d9e"
	leaf := &x509.Certificate{URIs: []*url.URL{
		mustParseURL(t, "urn:wendy:org:2:asset:408"),
		mustParseURL(t, DeviceSPIFFEURI(tenant, "sh/wendy/2/408")),
		mustParseURL(t, "spiffe://wendy.sh/tenant/"+tenant+"/service/asset-408"),
	}}
	got := TenantSPIFFEURIs(leaf)
	if len(got) != 2 {
		t.Fatalf("TenantSPIFFEURIs returned %d SANs, want the 2 tenant ones: %v", len(got), got)
	}
	if got[0] != DeviceSPIFFEURI(tenant, "sh/wendy/2/408") {
		t.Errorf("order was not preserved: %v", got)
	}
	// The urn form is not a tenant SPIFFE URI and must not be reported as one.
	for _, u := range got {
		if strings.HasPrefix(u, "urn:") {
			t.Errorf("the urn SAN leaked into the SPIFFE list: %v", got)
		}
	}
	if TenantSPIFFEURIs(&x509.Certificate{}) != nil {
		t.Error("a certificate with no SANs must yield nil")
	}
}
