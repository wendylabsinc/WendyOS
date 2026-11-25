package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

func cacheDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func publishLargePackage(t *testing.T, p *packageTestServer) (sitepub.Publication, []byte, string) {
	t.Helper()
	raw, err := os.ReadFile("../examples/live-office/world.json")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	model, err := os.ReadFile("../examples/live-office/office.xml")
	if err != nil {
		t.Fatal(err)
	}
	model = append(model, bytes.Repeat([]byte("\n<!-- Synthetic download-resume fixture; never executed. -->"), 3000)...)
	root["mujoco"].(map[string]any)["sha256"] = cacheDigest(model)
	// Populate the real evidence store with the large fixture. The pre-existing
	// package test server's upload decoder has a smaller bound than the actual
	// node handler. This feature exercises download and installation, not upload.
	put := evidence.PutRequest{SHA256: cacheDigest(model), Size: int64(len(model)), ContentType: packageContentType}
	for offset := 0; offset < len(model); {
		end := min(len(model), offset+evidence.MaxChunk)
		put.Offset, put.Data = int64(offset), model[offset:end]
		stored, err := p.blobs.Put(p.operator.Grant.Grant.Principal, put)
		if err != nil {
			t.Fatal(err)
		}
		put.RetainUntilUnixMS = stored.RetainUntilUnixMS
		offset = end
	}
	packagePath := filepath.Join(p.dir, "large-world.json")
	writeSiteJSON(t, packagePath, root)
	if err = os.WriteFile(filepath.Join(p.dir, "office.xml"), model, 0600); err != nil {
		t.Fatal(err)
	}
	rootBytes, _ := os.ReadFile(packagePath)
	siteRaw, _ := os.ReadFile(p.site)
	var site map[string]any
	json.Unmarshal(siteRaw, &site)
	site["package_sha256"] = cacheDigest(rootBytes)
	writeSiteJSON(t, p.site, site)
	var out bytes.Buffer
	if err = runPackage([]string{"publish", "--credentials", p.operatorFile, "--site", p.site, "--package", packagePath, "--run-id", "resumable-package-fixture", "--ttl", "5m"}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Publication sitepub.Publication `json:"publication"`
	}
	if err = json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Publication, model, cacheDigest(model)
}

func TestPackageCacheResumeCLI(t *testing.T) {
	p := newPackageTestServer(t)
	publication, model, modelHash := publishLargePackage(t, p)
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	var offsets []int64
	original := p.server.Config.Handler
	p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/evidence/get" {
			raw, _ := io.ReadAll(r.Body)
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var get evidence.GetRequest
			json.Unmarshal(raw, &get)
			if get.SHA256 == modelHash {
				mu.Lock()
				offsets = append(offsets, get.Offset)
				mu.Unlock()
				if fail.Load() && get.Offset >= evidence.MaxChunk {
					http.Error(w, "simulated link interruption", 503)
					return
				}
			}
		}
		original.ServeHTTP(w, r)
	})
	cacheDir, target := filepath.Join(p.dir, "cache"), filepath.Join(p.dir, "installed")
	args := []string{"fetch", "--credentials", p.viewerFile, "--trust", p.trustFile, "--sha256", publication.SHA256, "--cache-dir", cacheDir, "--out", target, "--json"}
	type event struct {
		Command string `json:"command"`
		Output  string `json:"output"`
		Error   string `json:"error,omitempty"`
	}
	var transcript []event
	execute := func(args []string) ([]byte, error) {
		var out, stderr bytes.Buffer
		var err error
		if binary := os.Getenv("WORLD_PACKAGE_DEMO_BINARY"); binary != "" {
			cmd := exec.Command(binary, worldCLIArgs(append([]string{"package"}, args...))...)
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			err = cmd.Run()
		} else {
			err = runPackage(args, &out)
			if err != nil {
				stderr.WriteString(err.Error())
			}
		}
		printed := append([]string(nil), args...)
		for i := range printed {
			printed[i] = strings.ReplaceAll(printed[i], p.dir, "$PRIVATE_FIXTURE")
		}
		transcript = append(transcript, event{Command: "world package " + strings.Join(printed, " "), Output: out.String(), Error: stderr.String()})
		return out.Bytes(), err
	}
	if out, err := execute(args); err == nil || len(out) != 0 {
		t.Fatal("interrupted command claimed success", string(out), err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial installation exists", err)
	}
	list, err := execute([]string{"cache", "list", "--cache-dir", cacheDir, "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var inventory packagecache.Inventory
	if err = json.Unmarshal(list, &inventory); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range inventory.Entries {
		if entry.SHA256 == modelHash {
			found = true
			if entry.Received != evidence.MaxChunk || entry.Complete {
				t.Fatal(entry)
			}
		}
	}
	if !found {
		t.Fatal("partial model missing from cache inventory")
	}
	fail.Store(false)
	mu.Lock()
	offsets = nil
	mu.Unlock()
	resultRaw, err := execute(args)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Installed bool               `json:"installed"`
		Cache     packageCacheResult `json:"cache"`
	}
	if err = json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Installed || result.Cache.ResumedBytes != evidence.MaxChunk || result.Cache.Hits != 1 || result.Cache.DownloadedBytes != int64(len(model))-evidence.MaxChunk+1 {
		t.Fatal(string(resultRaw))
	}
	mu.Lock()
	first := offsets[0]
	mu.Unlock()
	if first != evidence.MaxChunk {
		t.Fatal("resumed read restarted at zero", first)
	}
	installed, err := worldpackage.Load(filepath.Join(target, "world.json"))
	if err != nil || !bytes.Equal(installed.Files["office.xml"], model) {
		t.Fatal("installed bytes differ", err)
	}
	if _, err = execute([]string{"cache", "list", "--cache-dir", cacheDir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, err = execute([]string{"cache", "discard", "--cache-dir", cacheDir, "--sha256", modelHash, "--json"}); err != nil {
		t.Fatal(err)
	}
	if artifact := os.Getenv("WORLD_PACKAGE_DEMO_TRANSCRIPT"); artifact != "" {
		if os.Getenv("WORLD_PACKAGE_DEMO_BINARY") == "" {
			t.Fatal("demo artifact requires actual CLI binary")
		}
		raw, _ := json.MarshalIndent(struct {
			Scenario string  `json:"scenario"`
			Events   []event `json:"events"`
		}{"Actual local CLI with synthetic signed package and an injected HTTP link interruption. No model execution or hardware.", transcript}, "", "  ")
		if err = os.WriteFile(artifact, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackageCacheCannotBypassRevocationOrExpiry(t *testing.T) {
	for _, kind := range []string{"revoked-site-origin", "expired-site", "revoked-evidence-reader"} {
		t.Run(kind, func(t *testing.T) {
			p := newPackageTestServer(t)
			publication := publishPackageTest(t, p)
			cacheDir := filepath.Join(p.dir, "cache")
			fetch := func(destination string) error {
				var out bytes.Buffer
				return runPackage([]string{"fetch", "--credentials", p.viewerFile, "--trust", p.trustFile, "--sha256", publication.SHA256, "--cache-dir", cacheDir, "--out", destination}, &out)
			}
			if err := fetch(filepath.Join(p.dir, "initial")); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "expired-site":
				p.mu.Lock()
				c := p.records[0].Component
				c.Timing.SampleTime = time.Now().Add(-time.Minute)
				c.TTL = time.Second
				record, err := authority.SignRecord(p.operator.Grant, ed25519.PrivateKey(p.operator.PrivateKey), c, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				p.records[0] = record
				p.mu.Unlock()
			case "revoked-site-origin":
				rev := p.trust.Revocations.Revocations
				rev.Revision++
				rev.GrantIDs = []string{p.operator.Grant.Grant.ID}
				signed, err := authority.SignRevocations(rev, p.root)
				if err != nil {
					t.Fatal(err)
				}
				p.trust.Revocations = signed
				writeSiteJSON(t, p.trustFile, p.trust)
			case "revoked-evidence-reader":
				cfg := p.viewer
				grant := cfg.Grant.Grant
				var scopes []string
				for _, scope := range grant.Scopes {
					if scope != "evidence-read" {
						scopes = append(scopes, scope)
					}
				}
				grant.Scopes = scopes
				var err error
				cfg.Grant, err = authority.SignGrant(grant, p.root)
				if err != nil {
					t.Fatal(err)
				}
				writeSiteJSON(t, p.viewerFile, cfg)
			}
			target := filepath.Join(p.dir, "forbidden")
			if err := fetch(target); err == nil {
				t.Fatal("cache bypassed current authority")
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unauthorized installation exists")
			}
		})
	}
}
