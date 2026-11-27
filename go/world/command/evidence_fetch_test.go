package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencecache"
)

func seedEvidence(t *testing.T, p *packageTestServer, data []byte, contentType string, until time.Time) client.EvidenceReference {
	t.Helper()
	if err := os.Chmod(p.dir, 0700); err != nil {
		t.Fatal(err)
	}
	ref := client.EvidenceReference{SHA256: cacheDigest(data), Size: int64(len(data)), ContentType: contentType}
	for offset := 0; offset < len(data); {
		end := min(len(data), offset+evidence.MaxChunk)
		_, err := p.blobs.Put(p.operator.Grant.Grant.Principal, evidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, ContentType: contentType, RetainUntilUnixMS: until.UnixMilli(), Offset: int64(offset), Data: data[offset:end]})
		if err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	return ref
}
func evidenceFiles(t *testing.T, dir string, ref client.EvidenceReference, servers ...*packageTestServer) (string, string) {
	t.Helper()
	sources := evidenceSourceFiles{}
	for i, p := range servers {
		sources.Sources = append(sources.Sources, struct {
			ID          string `json:"id"`
			Credentials string `json:"credentials"`
		}{[]string{"capture-a", "replica-b"}[i], p.viewerFile})
	}
	sp, rp := filepath.Join(dir, "sources.json"), filepath.Join(dir, "reference.json")
	writeSiteJSON(t, sp, sources)
	writeSiteJSON(t, rp, ref)
	return sp, rp
}
func TestEvidenceFetchResumeFallbackCLI(t *testing.T) {
	a, b := newPackageTestServer(t), newPackageTestServer(t)
	data := bytes.Repeat([]byte("synthetic inspection image bytes; not real imagery\n"), 4000)
	until := time.Now().Add(time.Hour)
	ref := seedEvidence(t, a, data, "image/png", until)
	seedEvidence(t, b, data, "image/png", until.Add(time.Minute))
	var modeA, modeB atomic.Int64 // 0 partial/offline; 1 healthy; 2 disconnected.
	var mu sync.Mutex
	var offsetsA, offsetsB []int64
	wrap := func(p *packageTestServer, mode *atomic.Int64, offsets *[]int64, partial bool) {
		original := p.server.Config.Handler
		p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/evidence/get" {
				raw, _ := io.ReadAll(r.Body)
				r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var q evidence.GetRequest
				json.Unmarshal(raw, &q)
				mu.Lock()
				*offsets = append(*offsets, q.Offset)
				mu.Unlock()
				m := mode.Load()
				if m == 2 || m == 0 && (!partial || q.Offset >= evidence.MaxChunk) {
					http.Error(w, "modeled partition", 503)
					return
				}
			}
			original.ServeHTTP(w, r)
		})
	}
	wrap(a, &modeA, &offsetsA, true)
	wrap(b, &modeB, &offsetsB, false)
	sourceFile, refFile := evidenceFiles(t, a.dir, ref, a, b)
	cache := filepath.Join(a.dir, "evidence-cache")
	target := filepath.Join(a.dir, "inspection.png")
	args := []string{"fetch", "--sources", sourceFile, "--reference", refFile, "--cache-dir", cache, "--out", target, "--json"}
	type event struct {
		Command string `json:"command"`
		Output  string `json:"output"`
		Error   string `json:"error,omitempty"`
	}
	var transcript []event
	execute := func(argv []string) ([]byte, error) {
		var stdout, stderr bytes.Buffer
		var err error
		if binary := os.Getenv("WORLD_EVIDENCE_FETCH_DEMO_BINARY"); binary != "" {
			c := exec.Command(binary, worldCLIArgs(append([]string{"evidence"}, argv...))...)
			c.Stdout = &stdout
			c.Stderr = &stderr
			err = c.Run()
		} else {
			err = runEvidence(argv, &stdout)
			if err != nil {
				stderr.WriteString(err.Error())
			}
		}
		cmd := "world evidence " + strings.Join(argv, " ")
		cmd = strings.ReplaceAll(cmd, a.dir, "$PRIVATE_A")
		cmd = strings.ReplaceAll(cmd, b.dir, "$PRIVATE_B")
		// Credentials are never recorded. Endpoint addresses remain private local artifacts.
		printed := strings.ReplaceAll(stdout.String(), a.dir, "$PRIVATE_A")
		printed = strings.ReplaceAll(printed, b.dir, "$PRIVATE_B")
		transcript = append(transcript, event{cmd, printed, stderr.String()})
		return stdout.Bytes(), err
	}
	raw, err := execute(args)
	if err == nil {
		t.Fatal("partition succeeded")
	}
	var result client.EvidenceResolution
	if json.Unmarshal(raw, &result) != nil || result.Status != "unavailable" || len(result.Attempts) != 2 || result.Attempts[0].DownloadedBytes != evidence.MaxChunk {
		t.Fatal(string(raw), err)
	}
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial output exists")
	}
	raw, err = execute([]string{"cache", "list", "--cache-dir", cache})
	if err != nil {
		t.Fatal(err)
	}
	var inv evidencecache.Inventory
	if json.Unmarshal(raw, &inv) != nil || inv.StoredBytes != evidence.MaxChunk || inv.ReservedBytes != ref.Size {
		t.Fatal(string(raw))
	}
	modeA.Store(1)
	mu.Lock()
	offsetsA = nil
	mu.Unlock()
	raw, err = execute(args)
	if err != nil {
		t.Fatal(string(raw), err)
	}
	json.Unmarshal(raw, &result)
	mu.Lock()
	first := offsetsA[0]
	mu.Unlock()
	if result.Source != "capture-a" || result.Attempts[0].ResumedBytes != evidence.MaxChunk || first != evidence.MaxChunk {
		t.Fatal(string(raw), first)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, data) {
		t.Fatal("export bytes differ")
	}
	before := append([]byte(nil), got...)
	if _, err = execute(args); err == nil {
		t.Fatal("overwrote existing output")
	}
	got, _ = os.ReadFile(target)
	if !bytes.Equal(before, got) {
		t.Fatal("overwritten")
	}
	modeA.Store(2)
	modeB.Store(1)
	args[len(args)-2] = filepath.Join(a.dir, "replica.png")
	raw, err = execute(args)
	if err != nil {
		t.Fatal(string(raw), err)
	}
	json.Unmarshal(raw, &result)
	if result.Source != "replica-b" || result.Attempts[0].Outcome != "unavailable" || result.Attempts[1].ResumedBytes != 0 || result.RetainUntilUnixMS != until.Add(time.Minute).UnixMilli() {
		t.Fatal(string(raw))
	}

	args[len(args)-2] = filepath.Join(a.dir, "cached.png")
	raw, err = execute(args)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &result)
	if !result.Attempts[1].CacheHit || result.Attempts[1].DownloadedBytes != 3 {
		t.Fatal("cache hit was not reauthorized", string(raw))
	}
	// A denied source cannot veto a separate source's successful authorization.
	revA := a.trust.Revocations.Revocations
	revA.Revision++
	revA.GrantIDs = []string{a.viewer.Grant.Grant.ID}
	signedA, e := authority.SignRevocations(revA, a.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.verifier.UpdateRevocations(signedA); e != nil {
		t.Fatal(e)
	}
	modeA.Store(1)
	args[len(args)-2] = filepath.Join(a.dir, "authorized-replica.png")
	raw, err = execute(args)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &result)
	if result.Source != "replica-b" || result.Attempts[0].Outcome != "denied" || result.Attempts[1].Outcome != "verified" {
		t.Fatal("separate source authority conflated", string(raw))
	}
	for _, p := range []*packageTestServer{b} {
		rev := p.trust.Revocations.Revocations
		rev.Revision++
		rev.GrantIDs = []string{p.viewer.Grant.Grant.ID}
		signed, e := authority.SignRevocations(rev, p.root)
		if e != nil {
			t.Fatal(e)
		}
		if e = p.verifier.UpdateRevocations(signed); e != nil {
			t.Fatal(e)
		}
	}
	modeA.Store(1)
	args[len(args)-2] = filepath.Join(a.dir, "revoked.png")
	raw, err = execute(args)
	if err == nil {
		t.Fatal("revoked cache exported")
	}
	json.Unmarshal(raw, &result)
	if result.Status != "denied" || result.Attempts[0].Outcome != "denied" || result.Attempts[1].Outcome != "denied" {
		t.Fatal(string(raw))
	}
	if _, err = os.Lstat(args[len(args)-2]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("revoked output exists")
	}
	if _, err = execute([]string{"cache", "discard", "--cache-dir", cache, "--source", "capture-a", "--sha256", ref.SHA256}); err != nil {
		t.Fatal(err)
	}
	raw, err = execute([]string{"cache", "list", "--cache-dir", cache})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &inv)
	if len(inv.Sources[0].Entries) != 0 || len(inv.Sources[1].Entries) != 1 {
		t.Fatal("discard crossed sources", string(raw))
	}
	if path := os.Getenv("WORLD_EVIDENCE_FETCH_DEMO_TRANSCRIPT"); path != "" {
		raw, _ := json.MarshalIndent(transcript, "", "  ")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceFetchRefusalsAndFinalAuthority(t *testing.T) {
	for _, variant := range []string{"size", "mime", "digest", "cross-site", "missing-scope", "changed-retention", "expired-response", "corrupt", "redirect", "revoked-before-export"} {
		t.Run(variant, func(t *testing.T) {
			p := newPackageTestServer(t)
			data := bytes.Repeat([]byte("synthetic audio bytes\n"), 5000)
			ref := seedEvidence(t, p, data, "audio/wav", time.Now().Add(time.Hour))
			sp, rp := evidenceFiles(t, p.dir, ref, p)
			target := filepath.Join(p.dir, "output.wav")
			cache := filepath.Join(p.dir, "cache")
			var requests atomic.Int64
			original := p.server.Config.Handler
			p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				if variant == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/forbidden", 302)
					return
				}
				if variant == "revoked-before-export" && n == int64((len(data)+evidence.MaxChunk-1)/evidence.MaxChunk)+2 {
					rev := p.trust.Revocations.Revocations
					rev.Revision++
					rev.GrantIDs = []string{p.viewer.Grant.Grant.ID}
					signed, _ := authority.SignRevocations(rev, p.root)
					p.verifier.UpdateRevocations(signed)
				}
				if variant == "changed-retention" || variant == "expired-response" {
					rec := httptest.NewRecorder()
					original.ServeHTTP(rec, r)
					var got evidence.GetResponse
					json.Unmarshal(rec.Body.Bytes(), &got)
					if variant == "expired-response" {
						got.RetainUntilUnixMS = 1
					} else if n > 1 {
						got.RetainUntilUnixMS++
					}
					json.NewEncoder(w).Encode(got)
					return
				}
				original.ServeHTTP(w, r)
			})
			switch variant {
			case "size":
				ref.Size++
				writeSiteJSON(t, rp, ref)
			case "mime":
				ref.ContentType = "video/mp4"
				writeSiteJSON(t, rp, ref)
			case "digest":
				ref.SHA256 = strings.Repeat("f", 64)
				writeSiteJSON(t, rp, ref)
			case "corrupt":
				p.corrupt = ref.SHA256
			case "missing-scope":
				cfg := p.viewer
				g := cfg.Grant.Grant
				g.Scopes = []string{"read"}
				cfg.Grant, _ = authority.SignGrant(g, p.root)
				writeSiteJSON(t, p.viewerFile, cfg)
			case "cross-site":
				other := newPackageTestServer(t)
				cfg := other.viewer
				g := cfg.Grant.Grant
				g.Site = "foreign"
				cfg.Grant, _ = authority.SignGrant(g, other.root)
				writeSiteJSON(t, other.viewerFile, cfg)
				sp, rp = evidenceFiles(t, p.dir, ref, p, other)
			}
			var out bytes.Buffer
			err := runEvidence([]string{"fetch", "--sources", sp, "--reference", rp, "--cache-dir", cache, "--out", target}, &out)
			if err == nil {
				t.Fatal("invalid retrieval accepted", out.String())
			}
			if variant == "revoked-before-export" && requests.Load() != int64((len(data)+evidence.MaxChunk-1)/evidence.MaxChunk)+2 {
				t.Fatal("final export authority fence was not exercised", requests.Load(), err)
			}
			if _, e := os.Lstat(target); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("failed fetch exported")
			}
			if (variant == "cross-site" || variant == "missing-scope") && requests.Load() != 0 {
				t.Fatal("invalid source scope made network calls")
			}
		})
	}
}
