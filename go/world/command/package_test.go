package command

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

type packageTestServer struct {
	*siteTestServer
	blobs            *evidence.Store
	corrupt, missing string
	pause            bool
	gets             int
}

func newPackageTestServer(t *testing.T) *packageTestServer {
	t.Helper()
	x := newSiteTestServer(t)
	p := &packageTestServer{siteTestServer: x}
	var err error
	p.blobs, err = evidence.Open(filepath.Join(x.dir, "blobs"), evidence.Config{Tenant: "lab", Site: "synthetic-office"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.blobs.Close() })
	for _, item := range []struct {
		path     string
		operator bool
	}{{x.operatorFile, true}, {x.viewerFile, false}} {
		cfg := x.viewer
		if item.operator {
			cfg = x.operator
		}
		g := cfg.Grant.Grant
		g.Scopes = append(g.Scopes, "evidence-read")
		if item.operator {
			g.Scopes = append(g.Scopes, "evidence-write")
		}
		cfg.Grant, err = authority.SignGrant(g, x.root)
		if err != nil {
			t.Fatal(err)
		}
		writeSiteJSON(t, item.path, cfg)
		if item.operator {
			x.operator = cfg
		} else {
			x.viewer = cfg
		}
	}
	original := x.server.Config.Handler
	x.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/evidence/put" && r.URL.Path != "/v1/evidence/get" {
			original.ServeHTTP(w, r)
			return
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		x.requests++
		body, _ := io.ReadAll(r.Body)
		header, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var req authority.SignedRequest
		if err == nil {
			err = authority.DecodeStrict(header, &req)
		}
		operation := "evidence-read"
		if r.URL.Path == "/v1/evidence/put" {
			operation = "evidence-write"
		}
		var identity authority.Identity
		if err == nil {
			identity, err = x.verifier.Verify(req, operation, body)
		}
		if err != nil {
			http.Error(w, err.Error(), 401)
			return
		}
		if operation == "evidence-write" {
			var put evidence.PutRequest
			if err = authority.DecodeStrict(body, &put); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			out, err := p.blobs.Put(identity.Principal, put)
			if err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		var get evidence.GetRequest
		if err = authority.DecodeStrict(body, &get); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		p.gets++
		if p.pause {
			time.Sleep(1100 * time.Millisecond)
		}
		if p.missing == get.SHA256 {
			http.Error(w, "missing", 404)
			return
		}
		out, err := p.blobs.Get(get)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		if p.corrupt == get.SHA256 {
			out.Data[0] ^= 1
		}
		json.NewEncoder(w).Encode(out)
	})
	return p
}
func publishPackageTest(t *testing.T, p *packageTestServer) sitepub.Publication {
	t.Helper()
	var out bytes.Buffer
	if err := runPackage([]string{"publish", "--credentials", p.operatorFile, "--site", p.site, "--package", "../examples/live-office/world.json", "--run-id", "package-test", "--ttl", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Publication sitepub.Publication `json:"publication"`
		Published   bool                `json:"published"`
		Files       int                 `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Published || result.Files != 2 {
		t.Fatal(out.String(), err)
	}
	return result.Publication
}
func fetchPackageTest(p *packageTestServer, digest, destination string, out io.Writer) error {
	return runPackage([]string{"fetch", "--credentials", p.viewerFile, "--trust", p.trustFile, "--sha256", digest, "--out", destination}, out)
}
func TestPackagePublishFetchVerifiedInstallation(t *testing.T) {
	p := newPackageTestServer(t)
	publication := publishPackageTest(t, p)
	target := filepath.Join(p.dir, "installed")
	var out bytes.Buffer
	if err := fetchPackageTest(p, publication.SHA256, target, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Verified, Installed bool
		PackageSHA256       string `json:"package_sha256"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Verified || !result.Installed || result.PackageSHA256 != publication.PackageSHA256 {
		t.Fatal(out.String(), err)
	}
	bundle, err := worldpackage.Load(filepath.Join(target, "world.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Manifest.SHA256 != publication.PackageSHA256 {
		t.Fatal("installed package differs")
	}
	before, _ := os.ReadFile(filepath.Join(target, "world.json"))
	out.Reset()
	if err = fetchPackageTest(p, publication.SHA256, target, &out); err == nil {
		t.Fatal("existing install overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(target, "world.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("existing files altered")
	}
}
func TestPackageRefusesCorruptMissingAndExpiredTransfers(t *testing.T) {
	for _, variant := range []string{"root-corrupt", "model-corrupt", "model-missing", "expired-during-transfer"} {
		t.Run(variant, func(t *testing.T) {
			p := newPackageTestServer(t)
			publication := publishPackageTest(t, p)
			bundle, err := worldpackage.Load("../examples/live-office/world.json")
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "root-corrupt":
				p.corrupt = bundle.Manifest.SHA256
			case "model-corrupt":
				p.corrupt = bundle.Manifest.Files[0].SHA256
			case "model-missing":
				p.missing = bundle.Manifest.Files[0].SHA256
			case "expired-during-transfer":
				c := p.records[0].Component
				c.Timing.SampleTime = time.Now()
				c.TTL = time.Second
				p.records[0], err = authority.SignRecord(p.operator.Grant, ed25519.PrivateKey(p.operator.PrivateKey), c, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				p.pause = true
			}
			target := filepath.Join(p.dir, "rejected")
			var out bytes.Buffer
			if err = fetchPackageTest(p, publication.SHA256, target, &out); err == nil || out.Len() != 0 {
				t.Fatal("invalid package accepted")
			}
			if variant == "expired-during-transfer" && p.gets == 0 {
				t.Fatal("expiration test did not begin transfer")
			}
			if _, err = os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed fetch created destination")
			}
		})
	}
}
func TestPackageBindingsCheckedBeforeUploadOrInstall(t *testing.T) {
	for _, variant := range []string{"digest", "site", "frame", "authority"} {
		t.Run(variant, func(t *testing.T) {
			p := newPackageTestServer(t)
			raw, _ := os.ReadFile(p.site)
			var cfg map[string]any
			json.Unmarshal(raw, &cfg)
			credentials := p.operatorFile
			switch variant {
			case "digest":
				cfg["package_sha256"] = strings.Repeat("a", 64)
			case "site":
				cfg["site"] = "elsewhere"
			case "frame":
				for _, frame := range cfg["frames"].([]any) {
					frame.(map[string]any)["ref"].(map[string]any)["revision"] = "different-frame"
				}
			case "authority":
				credentials = p.viewerFile
			}
			writeSiteJSON(t, p.site, cfg)
			var out bytes.Buffer
			err := runPackage([]string{"publish", "--credentials", credentials, "--site", p.site, "--package", "../examples/live-office/world.json", "--run-id", "package-test"}, &out)
			if err == nil || p.requests != 0 || out.Len() != 0 {
				t.Fatal("bad binding reached upload", err, p.requests)
			}
		})
	}
	p := newPackageTestServer(t)
	publication := publishPackageTest(t, p)
	wrong := p.viewer
	wrong.Grant.Grant.Site = "another-site"
	wrongFile := filepath.Join(p.dir, "wrong-blob-credentials.json")
	writeSiteJSON(t, wrongFile, wrong)
	var out bytes.Buffer
	err := runPackage([]string{"fetch", "--credentials", p.viewerFile, "--blob-credentials", wrongFile, "--trust", p.trustFile, "--sha256", publication.SHA256, "--out", filepath.Join(p.dir, "wrong-scope")}, &out)
	if err == nil || p.gets != 0 {
		t.Fatal("cross-site blob credentials accepted")
	}
}
func TestPackageReusesOtherOwnerBytes(t *testing.T) {
	p := newPackageTestServer(t)
	bundle, err := worldpackage.Load("../examples/live-office/world.json")
	if err != nil {
		t.Fatal(err)
	}
	// Allocate the root under another writer before the package publisher starts.
	req := evidence.PutRequest{SHA256: bundle.Manifest.SHA256, Size: int64(len(bundle.Root)), ContentType: packageContentType, Data: bundle.Root}
	stored, err := p.blobs.Put("another-writer", req)
	if err != nil {
		t.Fatal(err)
	}
	publishPackageTest(t, p)
	got, err := p.blobs.Get(evidence.GetRequest{SHA256: bundle.Manifest.SHA256, Length: evidence.MaxChunk})
	if err != nil {
		t.Fatal(err)
	}
	if got.RetainUntilUnixMS != stored.RetainUntilUnixMS || !bytes.Equal(got.Data, bundle.Root) {
		t.Fatal("reuse changed retention or bytes")
	}
}

func TestPackageRequiresPrivateCredentialFiles(t *testing.T) {
	p := newPackageTestServer(t)
	if err := os.Chmod(p.operatorFile, 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runPackage([]string{"publish", "--credentials", p.operatorFile, "--site", p.site, "--package", "../examples/live-office/world.json", "--run-id", "package-test"}, &out)
	if err == nil || p.requests != 0 || out.Len() != 0 {
		t.Fatal("nonprivate credentials accepted")
	}
}
