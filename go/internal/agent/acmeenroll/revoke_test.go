package acmeenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRevokeExistingAccountOnly(t *testing.T) {
	for _, problem := range []string{"", "alreadyRevoked", "unauthorized", "serverInternal"} {
		t.Run(problem, func(t *testing.T) {
			var base string
			var lookups, revocations atomic.Int32
			mux := http.NewServeMux()
			scope := "/" + testTenantID + "/acme"
			mux.HandleFunc(scope+"/directory", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, map[string]any{"newNonce": base + scope + "/new-nonce", "newAccount": base + scope + "/new-account", "newOrder": base + scope + "/new-order", "revokeCert": base + scope + "/revoke-cert"})
			})
			mux.HandleFunc(scope+"/new-nonce", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Replay-Nonce", "bm9uY2U")
				w.WriteHeader(200)
			})
			mux.HandleFunc(scope+"/new-account", func(w http.ResponseWriter, r *http.Request) {
				lookups.Add(1)
				var payload map[string]any
				decodePayload(t, r, &payload)
				if payload["onlyReturnExisting"] != true || payload["externalAccountBinding"] != nil {
					t.Error("registration/EAB attempted")
				}
				w.Header().Set("Replay-Nonce", "bm9uY2U")
				w.Header().Set("Location", base+scope+"/acct/1")
				writeJSON(w, 200, map[string]any{"status": "valid"})
			})
			mux.HandleFunc(scope+"/revoke-cert", func(w http.ResponseWriter, r *http.Request) {
				revocations.Add(1)
				var jws struct {
					Protected string `json:"protected"`
					Payload   string `json:"payload"`
				}
				if json.NewDecoder(r.Body).Decode(&jws) != nil {
					t.Error("bad JWS")
				}
				header, _ := base64.RawURLEncoding.DecodeString(jws.Protected)
				var h map[string]any
				_ = json.Unmarshal(header, &h)
				if h["kid"] != base+scope+"/acct/1" || h["jwk"] != nil {
					t.Error("not signed by existing account")
				}
				raw, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
				var payload struct {
					Certificate string `json:"certificate"`
					Reason      int    `json:"reason"`
				}
				_ = json.Unmarshal(raw, &payload)
				der, err := base64.RawURLEncoding.DecodeString(payload.Certificate)
				if err != nil || len(der) == 0 || payload.Reason != 5 {
					t.Error("wrong revocation payload")
				}
				w.Header().Set("Replay-Nonce", "bm9uY2U")
				if problem != "" {
					writeJSON(w, 400, map[string]any{"type": "urn:ietf:params:acme:error:" + problem, "detail": "sensitive backend diagnostic"})
					return
				}
				w.WriteHeader(200)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			base = server.URL
			cfg := Config{DirectoryURL: base + scope + "/directory", DeviceID: testDeviceID}
			principal, _ := cfg.PrincipalURI()
			uri, _ := url.Parse(principal)
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
			if err != nil {
				t.Fatal(err)
			}
			certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			path := filepath.Join(t.TempDir(), "account.pem")
			scoped := scopedAccountKeyPath(path, cfg)
			if _, err := loadOrCreateAccountKey(scoped); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(scoped)
			err = Revoke(context.Background(), cfg, path, certificate)
			success := problem == "" || problem == "alreadyRevoked"
			if (err == nil) != success {
				t.Fatalf("result %v", err)
			}
			if lookups.Load() != 1 || revocations.Load() != 1 {
				t.Fatalf("unexpected calls %d/%d", lookups.Load(), revocations.Load())
			}
			after, _ := os.ReadFile(scoped)
			if string(before) != string(after) {
				t.Fatal("account key changed")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("legacy key created")
			}
		})
	}
}

func TestRevokeMissingKeyDoesNotCreateAccount(t *testing.T) {
	cfg := Config{DirectoryURL: "https://acme.example/" + testTenantID + "/acme/directory", DeviceID: testDeviceID}
	// Bad certificate must stop before touching any account material or network.
	path := filepath.Join(t.TempDir(), "account.pem")
	if Revoke(context.Background(), cfg, path, "invalid") == nil {
		t.Fatal("invalid certificate accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("key created")
	}
	if _, err := os.Stat(path + ".d"); !os.IsNotExist(err) {
		t.Fatal("scoped key directory created")
	}
}
