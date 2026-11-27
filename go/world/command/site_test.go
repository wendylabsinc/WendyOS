package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type siteTestServer struct {
	mu                                             sync.Mutex
	records                                        []authority.SignedRecord
	epoch                                          uint64
	requests                                       int
	verifier                                       *authority.Verifier
	root                                           ed25519.PrivateKey
	operator                                       client.Config
	viewer                                         client.Config
	trust                                          siteTrust
	server                                         *httptest.Server
	dir, site, operatorFile, viewerFile, trustFile string
}

func writeSiteJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func newSiteTestServer(t *testing.T) *siteTestServer {
	t.Helper()
	x := &siteTestServer{dir: t.TempDir()}
	root, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	x.root = key
	now := time.Now()
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, key)
	if e != nil {
		t.Fatal(e)
	}
	x.trust = siteTrust{RootPublicKey: root, Revocations: rev}
	x.verifier, e = authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Audience: "node-1", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if e != nil {
		t.Fatal(e)
	}
	if e = x.verifier.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
	grant := func(name, kind string, scopes []string) client.Config {
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		owner := ""
		if siteScope(scopes, "publish") {
			owner = "node-1"
		}
		signed, err := authority.SignGrant(authority.Grant{Version: 1, ID: name, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Principal: name, Kind: kind, PublishAudience: owner, Source: world.Source{Device: name, App: "world", Service: name, Stream: "site"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: now.Add(-time.Second).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, key)
		if err != nil {
			t.Fatal(err)
		}
		return client.Config{Audience: "node-1", Grant: signed, PrivateKey: private, AllowInsecureSimulation: true}
	}
	x.operator = grant("site-admin", "operator", []string{"publish", "site-publish", "read"})
	x.viewer = grant("viewer", "operator", []string{"read"})
	x.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.requests++
		body, _ := io.ReadAll(r.Body)
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var request authority.SignedRequest
		if err == nil {
			err = authority.DecodeStrict(header, &request)
		}
		operation := "read"
		if r.Method == http.MethodPost {
			operation = "publish"
		}
		if err == nil {
			_, err = x.verifier.Verify(request, operation, body)
		}
		if err != nil {
			http.Error(w, err.Error(), 401)
			return
		}
		switch r.URL.Path {
		case "/v1/publishers/epoch":
			x.epoch++
			json.NewEncoder(w).Encode(map[string]uint64{"epoch": x.epoch})
		case "/v1/components", "/v1/packages/publish":
			var record authority.SignedRecord
			if err = authority.DecodeStrict(body, &record); err == nil {
				_, err = x.verifier.VerifyRecord(record)
			}
			if err == nil {
				_, err = sitepub.Validate(record.Component)
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			x.records = append(x.records, record)
			json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
		case "/v1/sites":
			json.NewEncoder(w).Encode(siteReply{Publications: append([]authority.SignedRecord{}, x.records...)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(x.server.Close)
	x.operator.URL = x.server.URL
	x.viewer.URL = x.server.URL
	x.operatorFile = filepath.Join(x.dir, "operator.json")
	x.viewerFile = filepath.Join(x.dir, "viewer.json")
	x.trustFile = filepath.Join(x.dir, "trust.json")
	x.site = filepath.Join(x.dir, "site.json")
	writeSiteJSON(t, x.operatorFile, x.operator)
	writeSiteJSON(t, x.viewerFile, x.viewer)
	writeSiteJSON(t, x.trustFile, x.trust)
	raw, e := os.ReadFile("../examples/geography/office.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["valid_from"] = now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	doc["valid_until"] = now.Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, f := range doc["frames"].([]any) {
		f.(map[string]any)["valid_from"] = doc["valid_from"]
		f.(map[string]any)["valid_until"] = doc["valid_until"]
	}
	writeSiteJSON(t, x.site, doc)
	return x
}
func publishSiteTest(t *testing.T, x *siteTestServer) sitepub.Publication {
	t.Helper()
	var output bytes.Buffer
	if err := runSite([]string{"publish", "--credentials", x.operatorFile, "--site", x.site, "--run-id", "site-test", "--ttl", "5m", "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Publication sitepub.Publication `json:"publication"`
		Published   bool                `json:"published"`
		Epoch       uint64              `json:"epoch"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || !response.Published || response.Epoch == 0 {
		t.Fatalf("bad publication output: %s %v", output.String(), err)
	}
	return response.Publication
}
func TestSitePublishListFetchVerifyOrigin(t *testing.T) {
	x := newSiteTestServer(t)
	publication := publishSiteTest(t, x)
	var out bytes.Buffer
	if err := runSite([]string{"list", "--credentials", x.viewerFile, "--trust", x.trustFile, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Publications []authority.SignedRecord `json:"publications"`
		Verified     bool                     `json:"verified"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || !reply.Verified || len(reply.Publications) != 1 {
		t.Fatalf("bad list: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runSite([]string{"fetch", "--credentials", x.viewerFile, "--trust", x.trustFile, "--sha256", publication.SHA256, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var fetched siteFetch
	if err := json.Unmarshal(out.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if !fetched.Verified || !bytes.Equal(fetched.Publication.Document, publication.Document) || fetched.SignedRecord.Component.Frame != sitepub.MetadataFrame() {
		t.Fatal("fetch altered signed document")
	}
	if _, err := x.verifier.VerifyRecord(fetched.SignedRecord); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSite([]string{"fetch", "--credentials", x.viewerFile, "--trust", x.trustFile, "--sha256", strings.Repeat("a", 64)}, &out); err == nil || out.Len() != 0 {
		t.Fatal("fetch guessed unknown digest")
	}
}
func TestSiteReadsRejectTamperingAndExpiredTrust(t *testing.T) {
	for _, variant := range []string{"signature", "grant", "ordinary-app", "expired-component", "revoked", "wrong-root", "expired-revocations"} {
		t.Run(variant, func(t *testing.T) {
			x := newSiteTestServer(t)
			publishSiteTest(t, x)
			switch variant {
			case "ordinary-app":
				grant := x.operator.Grant.Grant
				grant.Kind = "app"
				grant.Scopes = []string{"publish"}
				signed, err := authority.SignGrant(grant, x.root)
				if err != nil {
					t.Fatal(err)
				}
				record, err := authority.SignRecord(signed, ed25519.PrivateKey(x.operator.PrivateKey), x.records[0].Component, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				x.records[0] = record
			case "signature":
				x.records[0].Signature[0] ^= 1
			case "grant":
				x.records[0].Grant.Grant.Scopes = []string{"publish"}
			case "expired-component":
				c := x.records[0].Component
				c.Timing.SampleTime = time.Now().Add(-time.Second)
				c.TTL = time.Millisecond
				record, err := authority.SignRecord(x.operator.Grant, ed25519.PrivateKey(x.operator.PrivateKey), c, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				x.records[0] = record
			case "revoked":
				rev := x.trust.Revocations.Revocations
				rev.Revision++
				rev.GrantIDs = []string{x.operator.Grant.Grant.ID}
				x.trust.Revocations, _ = authority.SignRevocations(rev, x.root)
				writeSiteJSON(t, x.trustFile, x.trust)
			case "wrong-root":
				pub, _, _ := ed25519.GenerateKey(rand.Reader)
				x.trust.RootPublicKey = pub
				writeSiteJSON(t, x.trustFile, x.trust)
			case "expired-revocations":
				rev := x.trust.Revocations.Revocations
				rev.IssuedAtUnixMS = time.Now().Add(-time.Minute).UnixMilli()
				rev.ExpiresAtUnixMS = time.Now().Add(-time.Second).UnixMilli()
				x.trust.Revocations, _ = authority.SignRevocations(rev, x.root)
				writeSiteJSON(t, x.trustFile, x.trust)
			}
			var out bytes.Buffer
			if err := runSite([]string{"list", "--credentials", x.viewerFile, "--trust", x.trustFile}, &out); err == nil || out.Len() != 0 {
				t.Fatal("untrusted publication accepted")
			}
		})
	}
}
func TestSitePublicationRequiresNarrowAuthorityAndExplicitFlags(t *testing.T) {
	x := newSiteTestServer(t)
	bad := [][]string{
		{"publish", "--credentials", x.viewerFile, "--site", x.site, "--run-id", "site-test"},
		{"publish", "--credentials", x.operatorFile, "--site", x.site, "--run-id", "site-test", "--ttl", "25h"},
		{"publish", "--credentials", x.operatorFile, "--site", x.site},
		{"list", "--credentials", x.viewerFile},
		{"list", "--credentials", x.viewerFile, "--trust", x.trustFile, "--sha256", strings.Repeat("a", 64)},
		{"fetch", "--credentials", x.viewerFile, "--trust", x.trustFile, "--sha256", "latest"},
	}
	for _, args := range bad {
		var out bytes.Buffer
		if err := runSite(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid command accepted", args)
		}
	}
	if x.requests != 0 {
		t.Fatal("invalid requests reached server")
	}
}
