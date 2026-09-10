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

func TestTenantUUIDFromToken(t *testing.T) {
	const tenant = "022b7284-f7f3-4d86-b844-d105a7c06d9e"
	tok := makeToken(t, `{"type":"asset_enrollment","org_id":2,"asset_id":408,"tenant_uuid":"`+tenant+`"}`)
	got, ok := TenantUUIDFromToken(tok)
	if !ok {
		t.Fatal("ok = false for a token carrying tenant_uuid")
	}
	if got != tenant {
		t.Errorf("tenant = %q, want %q", got, tenant)
	}
}

func TestTenantUUIDFromTokenAbsentIsNotAnError(t *testing.T) {
	// Two ordinary states, deliberately indistinguishable because a caller
	// can do nothing different about either: an org with no pki tenant, and a
	// token that is not a Wendy JWT at all (pki-core's own enrollment tokens
	// are opaque values with no claims to read). Both mean "take the tenant
	// from the enrol input instead".
	for name, tok := range map[string]string{
		"no claim":     makeToken(t, `{"type":"asset_enrollment","org_id":2,"asset_id":408}`),
		"empty claim":  makeToken(t, `{"type":"asset_enrollment","tenant_uuid":""}`),
		"opaque token": "an-opaque-pki-core-token",
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := TenantUUIDFromToken(tok); ok {
				t.Errorf("ok = true, tenant = %q", got)
			}
		})
	}
}
