package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// These deterministic keys belong only to isolated test vectors. No generated
// lab or deployed node trusts this test root.
func nativeClientVectors(t *testing.T) any {
	t.Helper()
	rootSeed, clientSeed := make([]byte, 32), make([]byte, 32)
	for i := range rootSeed {
		rootSeed[i], clientSeed[i] = byte(i), byte(i+32)
	}
	root, key := ed25519.NewKeyFromSeed(rootSeed), ed25519.NewKeyFromSeed(clientSeed)
	issued := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	vectors := []map[string]any{}
	for _, escaped := range []bool{false, true} {
		name := "plain"
		g := Grant{Version: 1, ID: "native-reader", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/viewer", Kind: "operator", Source: world.Source{Device: "native-reader", App: "wendy-world", Service: "viewer", Stream: "read"}, PublicKey: key.Public().(ed25519.PublicKey), Scopes: []string{"read", "evidence-read"}, NotBeforeUnixMS: issued.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: issued.Add(time.Hour).UnixMilli()}
		if escaped {
			name = "escaped-optional"
			g.ID = "reader-é<>&\u2028\u2029"
			g.Principal = "lab/reader-東京<>&\u2028\u2029"
			g.Source.Service = "viewer/\"quoted\"\nline"
			g.PublishAudience = "unused<&>"
		}
		grant, err := SignGrant(g, root)
		if err != nil {
			t.Fatal(err)
		}
		body := []byte{}
		operation := "read"
		if escaped {
			body, operation = []byte(`{"sha256":"test-digest","offset":0,"length":65536}`), "evidence-read"
		}
		nonce := "native_fixture_nonce_0123456789"
		r, err := SignRequest(grant, key, "node-3", operation, body, nonce, issued, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		grantJSON, _ := json.Marshal(grant)
		signing, err := signingBytes("request", requestPayload(r))
		if err != nil {
			t.Fatal(err)
		}
		if !ed25519.Verify(g.PublicKey, signing, r.Signature) {
			t.Fatal("fixture signature invalid")
		}
		vectors = append(vectors, map[string]any{
			"name":        name,
			"credentials": map[string]any{"url": "http://127.0.0.1:1234", "audience": "node-3", "grant": grant, "private_key": []byte(key), "allow_insecure_simulation": true},
			"operation":   operation, "body_base64": body, "nonce": nonce,
			"issued_at_unix_ms": issued.UnixMilli(), "lifetime_ms": int64(time.Minute / time.Millisecond),
			"expected_signed_request": r, "expected_grant_json": string(grantJSON), "expected_signing_bytes_base64": signing,
		})
	}
	return map[string]any{"schema": "wendy.native-client.signing-fixtures.v1", "test_only": true, "root_public_key": root.Public().(ed25519.PublicKey), "vectors": vectors}
}

func TestNativeClientSigningFixtures(t *testing.T) {
	want, err := json.MarshalIndent(nativeClientVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	for _, relative := range []string{
		"tests/fixtures/native-request.json",
		"../macos/WorldClient/Tests/WorldClientTests/Fixtures/native-request.json",
		"clients/android/app/src/test/resources/native-request.json",
		"clients/android/app/src/androidTest/assets/native-request.json",
	} {
		path := filepath.Join("..", "..", relative)
		if os.Getenv("WENDY_WRITE_NATIVE_VECTORS") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from authority signing implementation", relative)
		}
	}
}
