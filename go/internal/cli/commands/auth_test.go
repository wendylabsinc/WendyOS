package commands

import (
	"encoding/base64"
	"testing"
)

func testEnrollmentToken(t *testing.T, payload string) string {
	t.Helper()
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestEnrollmentTokenCommonName_UserEnrollment(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"user_enrollment","org_id":1,"user_id":"user-123"}`)

	got, err := enrollmentTokenCommonName(token)
	if err != nil {
		t.Fatalf("enrollmentTokenCommonName() error = %v", err)
	}
	if got != "wendy/user/user-123" {
		t.Fatalf("enrollmentTokenCommonName() = %q, want %q", got, "wendy/user/user-123")
	}
}

func TestEnrollmentTokenCommonName_AssetEnrollment(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"asset_enrollment","org_id":7,"asset_id":42}`)

	got, err := enrollmentTokenCommonName(token)
	if err != nil {
		t.Fatalf("enrollmentTokenCommonName() error = %v", err)
	}
	if got != "wendy/7/42" {
		t.Fatalf("enrollmentTokenCommonName() = %q, want %q", got, "wendy/7/42")
	}
}

func TestEnrollmentTokenCommonName_InvalidToken(t *testing.T) {
	if _, err := enrollmentTokenCommonName("not-a-jwt"); err == nil {
		t.Fatal("expected invalid token error")
	}
}
