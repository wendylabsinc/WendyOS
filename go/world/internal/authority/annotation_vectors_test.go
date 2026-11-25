package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestNativeAnnotationSigningFixture(t *testing.T) {
	// Public deterministic test keys; no runtime or generated lab trusts them.
	rootSeed, keySeed := make([]byte, 32), make([]byte, 32)
	for i := range rootSeed {
		rootSeed[i] = byte(i)
		keySeed[i] = byte(i + 32)
	}
	root, key := ed25519.NewKeyFromSeed(rootSeed), ed25519.NewKeyFromSeed(keySeed)
	authored := time.Date(2026, 11, 10, 12, 0, 0, 120000000, time.UTC)
	g, err := SignGrant(Grant{PublishAudience: "node-3", Version: 1, ID: "native-contributor", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/operator-é<>&", Kind: "operator", Source: world.Source{Device: "native-contributor", App: "wendy-world", Service: "viewer", Stream: "annotations"}, PublicKey: key.Public().(ed25519.PublicKey), Scopes: []string{"read", "evidence-read", "annotation-write"}, NotBeforeUnixMS: authored.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: authored.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	note := annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: g.Grant.Principal, Text: "Check \"north\" door\n東京 <>&\u2028\u2029", PositionMM: [3]int64{1250, 0, -2500}, AuthoredAtUnixMS: authored.UnixMilli(), ExpiresAtUnixMS: authored.Add(time.Hour).UnixMilli()}
	component, err := annotations.Build(note, annotations.Config{Tenant: g.Grant.Tenant, Site: g.Grant.Site, RunID: "office-run", Source: g.Grant.Source, Frame: world.FrameRef{ID: "site", Revision: "office-v1"}, Epoch: 7, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := SignAnnotation(g, key, component)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(record)
	componentJSON, _ := json.Marshal(component)
	grantJSON, _ := json.Marshal(g)
	recordSigning, err := signingBytes("record", recordPayload(record))
	if err != nil {
		t.Fatal(err)
	}
	request, err := SignRequest(g, key, "node-3", "annotation-write", body, "annotation_fixture_nonce_0123456789", authored.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	requestSigning, err := signingBytes("request", requestPayload(request))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(g.Grant.PublicKey, recordSigning, record.Signature) {
		t.Fatal("invalid record vector")
	}
	vector := map[string]any{
		"schema": "wendy.native-client.annotation-fixtures.v1", "test_only": true, "root_public_key": root.Public().(ed25519.PublicKey),
		"credentials": map[string]any{"url": "http://127.0.0.1:1234", "audience": "node-3", "grant": g, "private_key": []byte(key), "allow_insecure_simulation": true},
		"note":        note, "run_id": "office-run", "frame": component.Frame, "epoch": uint64(7), "sequence": uint64(1),
		"expected_component": component, "expected_component_json": string(componentJSON), "expected_grant_json": string(grantJSON),
		"expected_signed_record": record, "expected_signed_record_json": string(body), "expected_record_signing_bytes_base64": recordSigning,
		"operation": "annotation-write", "request_issued_at_unix_ms": request.IssuedAtUnixMS, "request_lifetime_ms": int64(time.Minute / time.Millisecond), "nonce": request.Nonce,
		"expected_signed_request": request, "expected_request_signing_bytes_base64": requestSigning,
	}
	want, err := json.MarshalIndent(vector, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "tests", "fixtures", "native-annotation.json")
	if os.Getenv("WENDY_WRITE_ANNOTATION_VECTOR") == "1" {
		if err = os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("native-annotation.json differs from Go signing contract")
	}
}
