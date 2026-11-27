package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestAnnotationHelperPreservesPreparedBytesAndChecksAcknowledgment(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/native-annotation.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Credentials   Config          `json:"credentials"`
		Component     world.Component `json:"expected_component"`
		ComponentJSON string          `json:"expected_component_json"`
		Record        string          `json:"expected_signed_record_json"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	// The display object in the fixture is indented. Signed payloads use the
	// separate exact canonical JSON string, including nested raw payload bytes.
	if err = json.Unmarshal([]byte(fixture.ComponentJSON), &fixture.Component); err != nil {
		t.Fatal(err)
	}
	mode := "applied"
	var previousNonce string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err != nil {
			t.Error(err)
			return
		}
		var proof authority.SignedRequest
		if err = json.Unmarshal(header, &proof); err != nil {
			t.Error(err)
			return
		}
		if proof.Operation != "annotation-write" || proof.Nonce == previousNonce {
			t.Error("wrong scope or reused nonce")
		}
		previousNonce = proof.Nonce
		if r.URL.Path == "/v1/annotations/epoch" {
			if string(body) != "{}" {
				t.Error("unexpected epoch body")
			}
			_, _ = w.Write([]byte(`{"epoch":7}`))
			return
		}
		if r.URL.Path != "/v1/annotations" || !bytes.Equal(body, []byte(fixture.Record)) {
			t.Error("pending signed bytes changed")
		}
		response := AnnotationResult{Result: world.Result(mode), Component: fixture.Component}
		if mode == "foreign" {
			response.Result = world.Applied
			response.Component.Key.Source.Device = "other"
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	fixture.Credentials.URL = server.URL
	c, err := New(fixture.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	if epoch, err := c.NextAnnotationEpoch(context.Background()); err != nil || epoch != 7 {
		t.Fatal(epoch, err)
	}
	prepared, err := c.PrepareAnnotation(fixture.Component)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared, []byte(fixture.Record)) {
		t.Fatal("Go helper differs from signing vector")
	}
	for _, candidate := range []string{"applied", "duplicate", "obsolete", "foreign", ""} {
		mode = candidate
		_, err := c.PublishAnnotation(context.Background(), prepared)
		wantSuccess := mode == "applied" || mode == "duplicate"
		if (err == nil) != wantSuccess {
			t.Fatal(mode, err)
		}
	}
}
