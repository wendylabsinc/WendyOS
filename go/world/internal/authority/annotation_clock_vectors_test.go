package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestNativeAnnotationClockFixture(t *testing.T) {
	seed := func(offset int) ed25519.PrivateKey {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + offset)
		}
		return ed25519.NewKeyFromSeed(b)
	}
	root, key, serverKey := seed(0), seed(32), seed(64)
	now := time.Date(2026, 11, 10, 12, 0, 0, 120000000, time.UTC)
	g, err := SignGrant(Grant{PublishAudience: "node-3", Version: 1, ID: "native-contributor", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/operator-é<>&", Kind: "operator", Source: world.Source{Device: "native-contributor", App: "wendy-world", Service: "viewer", Stream: "annotations"}, PublicKey: key.Public().(ed25519.PublicKey), Scopes: []string{"read", "evidence-read", "annotation-write"}, NotBeforeUnixMS: now.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: now.Add(2 * time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	server, err := SignGrant(Grant{PublishAudience: "node-3", Version: 1, ID: "clock-server", Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: "lab/clock-node-3", Kind: "device", Source: world.Source{Device: "node-3", App: "wendy-world", Service: "clock", Stream: "annotations"}, PublicKey: serverKey.Public().(ed25519.PublicKey), Scopes: []string{"annotation-clock-server"}, NotBeforeUnixMS: now.Add(-time.Hour).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(Config{RootPublicKey: root.Public().(ed25519.PublicKey), Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: "node-3", Now: func() time.Time { return now }, Replay: NewMemoryReplay(100)})
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := SignRevocations(Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(59 * time.Minute).UnixMilli()}, root)
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	request, err := SignAnnotationClockRequest(g, key, annotationclock.Request{Tenant: "lab", Site: "synthetic-office", Audience: "node-3", RunID: "office-run", ClockDomain: "simulation:office-run", Nonce: strings.Repeat("ab", 32)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := v.IssueAnnotationClock(request, server, serverKey, "office-run", "simulation:office-run", strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	pins := annotationclock.Pins{RootPublicKey: root.Public().(ed25519.PublicKey), GatewayPrincipal: server.Grant.Principal, GatewayKeySHA256: KeyID(server.Grant.PublicKey), RunID: "office-run", ClockDomain: "simulation:office-run"}
	if _, err = VerifyAnnotationClockObservation(request, response, pins); err != nil {
		t.Fatal(err)
	}
	evidence := annotationclock.Evidence{Request: request, Observation: response, RTTNS: 123456789, ElapsedNS: 1234567890123}
	interval, err := evidence.Interval()
	if err != nil {
		t.Fatal(err)
	}
	note := annotations.Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: g.Grant.Principal, Text: "Clock-bound 東京 <>&\u2028\u2029", PositionMM: [3]int64{1250, 0, -2500}, AuthoredAtUnixMS: interval.LowerUnixMS, ExpiresAtUnixMS: interval.LowerUnixMS + 3600000, Clock: &evidence}
	component, err := annotations.Build(note, annotations.Config{Tenant: "lab", Site: "synthetic-office", RunID: "office-run", Source: g.Grant.Source, Frame: world.FrameRef{ID: "site", Revision: "office-v1"}, Epoch: 7, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	record, err := SignAnnotation(g, key, component)
	if err != nil {
		t.Fatal(err)
	}
	canonical := func(value any) string {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	requestSigning, _ := signingBytes("annotation-clock-request", annotationClockPayload(request.Version, request.Grant, request.Request))
	responseSigning, _ := signingBytes("annotation-clock-observation", annotationClockPayload(response.Version, response.Grant, response.Observation))
	recordSigning, _ := signingBytes("record", recordPayload(record))
	vector := map[string]any{"schema": "wendy.native-client.annotation-clock-fixtures.v1", "test_only": true, "pins": pins, "credentials": map[string]any{"url": "http://127.0.0.1:1234", "audience": "node-3", "grant": g, "private_key": []byte(key), "allow_insecure_simulation": true, "clock_trust": pins}, "request": request, "request_json": canonical(request), "request_signing_bytes_base64": requestSigning, "observation": response, "observation_json": canonical(response), "observation_signing_bytes_base64": responseSigning, "evidence": evidence, "interval": interval, "note": note, "run_id": "office-run", "frame": component.Frame, "epoch": 7, "sequence": 1, "expected_component": component, "expected_component_json": canonical(component), "expected_signed_record": record, "expected_signed_record_json": canonical(record), "expected_record_signing_bytes_base64": recordSigning, "revocations": rev}
	want, _ := json.MarshalIndent(vector, "", "  ")
	want = append(want, '\n')
	path := filepath.Join("..", "..", "tests", "fixtures", "native-annotation-clock.json")
	if os.Getenv("WENDY_WRITE_ANNOTATION_CLOCK_VECTOR") == "1" {
		if err = os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("annotation clock fixture differs")
	}
}
