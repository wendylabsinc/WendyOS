package command

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func evidenceCLIFixture(t *testing.T) (*siteTestServer, string, evidence.PinRequest) {
	t.Helper()
	x := newSiteTestServer(t)
	x.operator.Grant.Grant.Scopes = []string{"evidence-pin"}
	var err error
	x.operator.Grant, err = authority.SignGrant(x.operator.Grant.Grant, x.root)
	if err != nil {
		t.Fatal(err)
	}
	writeSiteJSON(t, x.operatorFile, x.operator)
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "lab", Site: "synthetic-office"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	data := []byte("camera sample")
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	until := time.Now().Add(time.Hour).UnixMilli()
	if _, err = store.Put("sensor", evidence.PutRequest{SHA256: digest, Size: int64(len(data)), Data: data, ContentType: "image/png", RetainUntilUnixMS: until, RetentionClass: "rolling"}); err != nil {
		t.Fatal(err)
	}
	x.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.requests++
		body, _ := io.ReadAll(r.Body)
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		if err == nil {
			err = authority.DecodeStrict(header, &signed)
		}
		var identity authority.Identity
		if err == nil {
			identity, err = x.verifier.Verify(signed, "evidence-pin", body)
		}
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		var result evidence.PinResponse
		switch r.URL.Path {
		case "/v1/evidence/pin":
			var request evidence.PinRequest
			err = authority.DecodeStrict(body, &request)
			if err == nil {
				result, err = store.Pin(identity.Principal, request)
			}
		case "/v1/evidence/unpin":
			var request evidence.UnpinRequest
			err = authority.DecodeStrict(body, &request)
			if err == nil {
				result, err = store.Unpin(identity.Principal, request)
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(result)
	})
	request := evidence.PinRequest{ID: "investigation-42", SHA256: []string{digest}, RetainUntilUnixMS: until - 1000}
	requestFile := filepath.Join(x.dir, "pin.json")
	writeSiteJSON(t, requestFile, request)
	return x, requestFile, request
}

func TestEvidenceCLIPinAndReleaseUseImmutableRequest(t *testing.T) {
	x, file, request := evidenceCLIFixture(t)
	args := []string{"pin", "--credentials", x.operatorFile, "--request", file, "--json"}
	for range 2 {
		var output bytes.Buffer
		if err := runEvidence(args, &output); err != nil {
			t.Fatal(err)
		}
		var result evidence.PinResponse
		if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.ID != request.ID || result.RetainUntilUnixMS != request.RetainUntilUnixMS || result.State != "active" {
			t.Fatal("unexpected pin receipt", output.String(), err)
		}
	}
	var output bytes.Buffer
	if err := runEvidence([]string{"unpin", "--credentials", x.operatorFile, "--id", request.ID, "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var released evidence.PinResponse
	if err := json.Unmarshal(output.Bytes(), &released); err != nil || released.State != "released" {
		t.Fatal("invalid release receipt", output.String(), err)
	}
	output.Reset()
	if err := runEvidence(args, &output); err == nil || output.Len() != 0 {
		t.Fatal("late command resurrected a released pin", output.String(), err)
	}
}

func TestEvidenceCLIRequiresExplicitPrivateDestinationBoundAuthority(t *testing.T) {
	for _, variant := range []string{"viewer", "wrong-audience", "agent", "public-credentials", "unknown-owner", "unknown-namespace", "unknown-flag"} {
		t.Run(variant, func(t *testing.T) {
			x, file, _ := evidenceCLIFixture(t)
			credentials := x.operatorFile
			args := []string{"pin", "--credentials", credentials, "--request", file}
			switch variant {
			case "viewer":
				args[2] = x.viewerFile
			case "wrong-audience":
				x.operator.Audience = "another-capture"
				writeSiteJSON(t, credentials, x.operator)
			case "agent":
				x.operator.Grant.Grant.Kind = "agent"
				writeSiteJSON(t, credentials, x.operator)
			case "public-credentials":
				if err := os.Chmod(credentials, 0644); err != nil {
					t.Fatal(err)
				}
			case "unknown-owner", "unknown-namespace":
				name := "owner"
				if variant == "unknown-namespace" {
					name = "namespace"
				}
				writeSiteJSON(t, file, map[string]string{"id": "case", name: "task"})
			case "unknown-flag":
				args = append(args, "--owner", "sensor")
			}
			var output bytes.Buffer
			if err := runEvidence(args, &output); err == nil || output.Len() != 0 {
				t.Fatal("invalid command accepted", variant, output.String(), err)
			}
			x.mu.Lock()
			requests := x.requests
			x.mu.Unlock()
			if requests != 0 {
				t.Fatal("invalid command issued network request", variant, requests)
			}
		})
	}
}
