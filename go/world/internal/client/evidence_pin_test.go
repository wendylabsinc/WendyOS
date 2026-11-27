package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func evidenceProtocolClient(t *testing.T, change func(*evidence.PinResponse)) (*Client, *evidence.Store, <-chan evidence.PutRequest) {
	t.Helper()
	rootPublic, root, _ := ed25519.GenerateKey(rand.Reader)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	g, err := authority.SignGrant(authority.Grant{Version: 1, ID: "evidence-client", Tenant: "tenant", Site: "site", Domain: "simulation", Principal: "investigator", Kind: "operator", PublishAudience: "capture", Source: world.Source{Device: "operator", App: "world", Service: "evidence", Stream: "pins"}, PublicKey: public, Scopes: []string{"evidence-pin", "evidence-write", "evidence-read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: rootPublic, Tenant: "tenant", Site: "site", Domain: "simulation", Audience: "capture", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "site", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, root)
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	store, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "site"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	requests := make(chan evidence.PutRequest, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		if err == nil {
			err = authority.DecodeStrict(header, &signed)
		}
		operation := "evidence-pin"
		if r.URL.Path == "/v1/evidence/put" {
			operation = "evidence-write"
		} else if r.URL.Path == "/v1/evidence/get" {
			operation = "evidence-read"
		}
		var identity authority.Identity
		if err == nil {
			identity, err = v.Verify(signed, operation, body)
		}
		if err != nil {
			http.Error(w, err.Error(), 401)
			return
		}
		var out any
		switch r.URL.Path {
		case "/v1/evidence/put":
			var request evidence.PutRequest
			err = authority.DecodeStrictLimit(body, &request, 1<<20)
			if err == nil {
				requests <- request
				out, err = store.Put(identity.Principal, request)
			}
		case "/v1/evidence/get":
			var request evidence.GetRequest
			err = authority.DecodeStrict(body, &request)
			if err == nil {
				out, err = store.Get(request)
			}
		case "/v1/evidence/pin", "/v1/evidence/unpin":
			var response evidence.PinResponse
			if strings.HasSuffix(r.URL.Path, "/pin") {
				var request evidence.PinRequest
				err = authority.DecodeStrict(body, &request)
				if err == nil {
					response, err = store.Pin(identity.Principal, request)
				}
			} else {
				var request evidence.UnpinRequest
				err = authority.DecodeStrict(body, &request)
				if err == nil {
					response, err = store.Unpin(identity.Principal, request)
				}
			}
			if change != nil && err == nil {
				change(&response)
			}
			out = response
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	c, err := New(Config{URL: server.URL, Audience: "capture", Grant: g, PrivateKey: private, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	return c, store, requests
}

func TestRollingEvidenceUploadsPreserveClassAndOriginalDeadline(t *testing.T) {
	c, _, requests := evidenceProtocolClient(t, nil)
	data := bytes.Repeat([]byte("camera"), 20000)
	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	out, err := c.UploadRollingEvidence(context.Background(), data, "image/png", until)
	if err != nil || !out.Complete || out.RetainUntilUnixMS != until.UnixMilli() {
		t.Fatal(out, err)
	}
	if _, err = c.UploadRollingEvidence(context.Background(), data, "image/png", until); err != nil {
		t.Fatal("completed rolling probe failed", err)
	}
	for count := len(requests); count > 0; count-- {
		request := <-requests
		if request.RetentionClass != "rolling" || request.RetainUntilUnixMS != until.UnixMilli() {
			t.Fatal("rolling resume changed metadata", request.RetentionClass, request.RetainUntilUnixMS)
		}
	}
	if _, err = c.UploadEvidence(context.Background(), data, "image/png", until); err == nil {
		t.Fatal("ordinary upload silently changed existing retention class")
	}
	protectedData := []byte("legacy protected capture")
	protected, err := c.UploadEvidence(context.Background(), protectedData, "image/png", until)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := c.UploadRollingEvidence(context.Background(), protectedData, "image/png", time.Time{})
	if err != nil || adopted != protected {
		t.Fatal("rolling probe failed to preserve completed protected metadata", adopted, protected, err)
	}
	if _, err = c.UploadRollingEvidence(context.Background(), protectedData, "image/png", until.Add(time.Hour)); err == nil {
		t.Fatal("rolling probe refreshed protected hard expiry")
	}
}

func TestPinClientValidatesBindingAndReleasedReceipt(t *testing.T) {
	for _, mutation := range []string{"valid", "id", "digest", "deadline", "released", "state"} {
		t.Run(mutation, func(t *testing.T) {
			change := func(pin *evidence.PinResponse) {
				switch mutation {
				case "id":
					pin.ID += "-other"
				case "digest":
					pin.SHA256[0] = strings.Repeat("0", 64)
				case "deadline":
					pin.RetainUntilUnixMS++
				case "released":
					pin.State = "released"
				case "state":
					pin.State = "unknown"
				}
			}
			c, store, _ := evidenceProtocolClient(t, change)
			data := []byte("recorded image")
			hash := sha256.Sum256(data)
			digest := hex.EncodeToString(hash[:])
			until := time.Now().Add(time.Hour).UnixMilli()
			if _, err := store.Put("other-publisher", evidence.PutRequest{SHA256: digest, Size: int64(len(data)), Data: data, ContentType: "image/png", RetainUntilUnixMS: until, RetentionClass: "rolling"}); err != nil {
				t.Fatal(err)
			}
			request := evidence.PinRequest{ID: "case", SHA256: []string{digest}, RetainUntilUnixMS: until - 1000}
			_, err := c.PinEvidence(context.Background(), request)
			if (err == nil) != (mutation == "valid") {
				t.Fatal("unexpected acknowledgement acceptance", mutation, err)
			}
			if mutation == "valid" {
				if _, err = c.UnpinEvidence(context.Background(), evidence.UnpinRequest{ID: request.ID}); err != nil {
					t.Fatal(err)
				}
				if result, err := c.PinEvidence(context.Background(), request); err == nil || result.State != "released" {
					t.Fatal("late create accepted as held", result, err)
				}
			}
		})
	}
}
