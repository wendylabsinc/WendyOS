package builddelivery

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Regenerate the public cross-language fixture with:
// WENDY_BUILD_DELIVERY_SWIFT_FIXTURE=/absolute/path/build-delivery-go.json \
// go test ./go/internal/shared/builddelivery -run TestWriteSwiftDeliveryFixture
// Only synthetic PUBLIC certificates and a JWS are written, never private keys.
func TestWriteSwiftDeliveryFixture(t *testing.T) {
	path := os.Getenv("WENDY_BUILD_DELIVERY_SWIFT_FIXTURE")
	if path == "" {
		t.Skip("set WENDY_BUILD_DELIVERY_SWIFT_FIXTURE to regenerate the Swift conformance fixture")
	}
	f := newFixtureAt(t, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	f.claims.Reference = "app:latest"
	compact := f.sign(t, f.claims)
	if _, err := Admit(compact, f.roots, f.target, f.now); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(map[string]any{"root_pem": f.chain, "target": f.target.Principal(), "builder_der": base64.StdEncoding.EncodeToString(f.builder.Raw), "authorization_jws": compact, "session": sessionID(compact), "verify_at": f.now.Unix()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
