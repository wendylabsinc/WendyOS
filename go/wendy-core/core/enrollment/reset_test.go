package enrollment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResetDeletionExtractionIsBoundedAndNotAuthority(t *testing.T) {
	raw := []byte{1, 2, 3}
	body, _ := json.Marshal(map[string]any{"deletion": raw})
	token := []byte("header." + base64.RawURLEncoding.EncodeToString(body) + ".signature")
	got, err := ResetDeletion(token)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("extraction: %v", err)
	}
	// Correctly shaped caller-supplied bytes still cannot become reset evidence.
	now := time.Now()
	if err := VerifySavedCloudReset(context.Background(), nil, token, nil, "api.example:443", "principal", "fingerprint", raw, nil, now, now); err == nil {
		t.Fatal("unverified deletion accepted")
	}
	for _, bad := range [][]byte{nil, []byte("a.b"), []byte(strings.Repeat("x", 65537)), []byte("a.!.c"), []byte("a." + base64.RawURLEncoding.EncodeToString([]byte(`{"deletion":"AQ==","deletion":"Ag=="}`)) + ".c"), []byte("a." + base64.RawURLEncoding.EncodeToString([]byte(`{"deletion":""}`)) + ".c")} {
		if _, err := ResetDeletion(bad); err == nil {
			t.Fatal("malformed claims accepted")
		}
	}
	oversized, _ := json.Marshal(map[string]any{"deletion": make([]byte, 4097)})
	if _, err := ResetDeletion([]byte("a." + base64.RawURLEncoding.EncodeToString(oversized) + ".c")); err == nil {
		t.Fatal("oversized deletion accepted")
	}
}
