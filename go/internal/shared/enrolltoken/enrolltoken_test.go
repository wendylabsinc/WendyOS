package enrolltoken

import (
	"encoding/base64"
	"testing"
)

// makeToken builds a fake JWT-shaped token: "<header>.<payloadJSON>.<sig>".
func makeToken(t *testing.T, payloadJSON string) string {
	t.Helper()
	seg := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	return "header." + seg + ".sig"
}

func TestParseAsset_Valid(t *testing.T) {
	tok := makeToken(t, `{"type":"asset_enrollment","org_id":7,"asset_id":42}`)
	orgID, assetID, err := ParseAsset(tok)
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	if orgID != 7 || assetID != 42 {
		t.Fatalf("got org=%d asset=%d, want 7/42", orgID, assetID)
	}
}

func TestParseAsset_RejectsUserToken(t *testing.T) {
	tok := makeToken(t, `{"type":"user_enrollment","org_id":1,"user_id":"u-1"}`)
	if _, _, err := ParseAsset(tok); err == nil {
		t.Fatal("expected error for user token, got nil")
	}
}

func TestParseAsset_Malformed(t *testing.T) {
	if _, _, err := ParseAsset("not-a-token"); err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestParseAsset_MissingIDs(t *testing.T) {
	tok := makeToken(t, `{"type":"asset_enrollment","org_id":0,"asset_id":0}`)
	if _, _, err := ParseAsset(tok); err == nil {
		t.Fatal("expected error for missing org/asset, got nil")
	}
}

func TestClaimsTenantSPIFFEURI(t *testing.T) {
	const tenant = "13a72725-dfe3-4425-bd04-b253d2036089"

	tests := []struct {
		name   string
		claims Claims
		want   string
		wantOK bool
	}{
		{
			name:   "asset enrollment with tenant",
			claims: Claims{Type: "asset_enrollment", OrganizationID: 7, AssetID: 42, TenantUUID: tenant},
			want:   "spiffe://wendy.sh/tenant/" + tenant + "/service/asset-42",
			wantOK: true,
		},
		{
			name:   "user enrollment with tenant",
			claims: Claims{Type: "user_enrollment", OrganizationID: 7, UserID: "user-123", TenantUUID: tenant},
			want:   "spiffe://wendy.sh/tenant/" + tenant + "/service/user-user-123",
			wantOK: true,
		},
		{
			// The normal state for local and GCP CAS backends: no pki tenant,
			// so no claim. This must stay a quiet "build the CSR as before",
			// never an error (WDY-2584).
			name:   "no tenant claim is not an error",
			claims: Claims{Type: "asset_enrollment", OrganizationID: 7, AssetID: 42},
			wantOK: false,
		},
		{
			name:   "asset enrollment missing asset id",
			claims: Claims{Type: "asset_enrollment", OrganizationID: 7, TenantUUID: tenant},
			wantOK: false,
		},
		{
			name:   "user enrollment missing user id",
			claims: Claims{Type: "user_enrollment", OrganizationID: 7, TenantUUID: tenant},
			wantOK: false,
		},
		{
			name:   "unknown token type",
			claims: Claims{Type: "something_else", TenantUUID: tenant},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.claims.TenantSPIFFEURI()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("uri = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseReadsTenantUUID(t *testing.T) {
	// The exact payload shape cloud documented on WDY-2584.
	payload := `{"jti":"0f9c1b2e-6a4d-4f18-9e77-2b5c8d3a4e10","org_id":7,"asset_id":42,` +
		`"tenant_uuid":"13a72725-dfe3-4425-bd04-b253d2036089","exp":1788290608,` +
		`"type":"asset_enrollment","wnd_typ":"enrollment"}`
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"

	c, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if c.TenantUUID != "13a72725-dfe3-4425-bd04-b253d2036089" {
		t.Fatalf("TenantUUID = %q, want the tenant_uuid claim", c.TenantUUID)
	}

	uri, ok := TenantSPIFFEURIFromToken(token)
	if !ok {
		t.Fatal("TenantSPIFFEURIFromToken() ok = false, want true")
	}
	if want := "spiffe://wendy.sh/tenant/13a72725-dfe3-4425-bd04-b253d2036089/service/asset-42"; uri != want {
		t.Fatalf("uri = %q, want %q", uri, want)
	}
}

func TestTenantSPIFFEURIFromTokenUndecodable(t *testing.T) {
	if _, ok := TenantSPIFFEURIFromToken("not-a-token"); ok {
		t.Fatal("an undecodable token must not yield a SPIFFE URI")
	}
}
