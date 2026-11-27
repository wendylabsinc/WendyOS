package command

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencemirror"
)

// Match the real node's 128 KiB chunk body limit. The older package fixture's
// generic DecodeStrict limit is too small for a full base64 48 KiB chunk.
func mirrorTestServer(t *testing.T) *packageTestServer {
	p := newPackageTestServer(t)
	original := p.server.Config.Handler
	p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/evidence/put" {
			original.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, (128<<10)+1))
		header, e := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var auth authority.SignedRequest
		if e == nil {
			e = authority.DecodeStrict(header, &auth)
		}
		var id authority.Identity
		if e == nil {
			id, e = p.verifier.Verify(auth, "evidence-write", body)
		}
		if e != nil {
			http.Error(w, e.Error(), 401)
			return
		}
		var q evidence.PutRequest
		if e = authority.DecodeStrictLimit(body, &q, 128<<10); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		out, e := p.blobs.Put(id.Principal, q)
		if e != nil {
			code := 409
			if errors.Is(e, evidence.ErrQuota) {
				code = 507
			}
			http.Error(w, e.Error(), code)
			return
		}
		json.NewEncoder(w).Encode(out)
	})
	return p
}
func mirrorCLI(t *testing.T, args ...string) (evidencemirror.Status, error) {
	t.Helper()
	var b bytes.Buffer
	e := runEvidenceMirror(args, &b)
	var s evidencemirror.Status
	if b.Len() > 0 && json.Unmarshal(b.Bytes(), &s) != nil {
		t.Fatal(b.String())
	}
	return s, e
}
func prepareMirror(t *testing.T, a, b *packageTestServer, data []byte) (string, time.Time) {
	t.Helper()
	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	ref := seedEvidence(t, a, data, "image/png", until)
	file := filepath.Join(a.dir, "reference.json")
	writeSiteJSON(t, file, ref)
	path := filepath.Join(a.dir, "mirror")
	s, e := mirrorCLI(t, "prepare", "--state-dir", path, "--id", "inspection-copy", "--source", a.viewerFile, "--destination", b.operatorFile, "--reference", file)
	if e != nil || s.Phase != "prepared" || s.Plan.RetainUntilUnixMS != until.UnixMilli() {
		t.Fatal(s, e)
	}
	return path, until
}
func TestMirrorResumesCommittedDestinationAfterLostAck(t *testing.T) {
	a, b := mirrorTestServer(t), mirrorTestServer(t)
	data := bytes.Repeat([]byte("synthetic image bytes, not physical capture\n"), 4000)
	original := b.server.Config.Handler
	var interrupt atomic.Bool
	interrupt.Store(true)
	var mu sync.Mutex
	var offsets []int64
	b.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/evidence/put" {
			original.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var q evidence.PutRequest
		json.Unmarshal(raw, &q)
		if len(q.Data) > 0 {
			mu.Lock()
			offsets = append(offsets, q.Offset)
			mu.Unlock()
		}
		if len(q.Data) > 0 && interrupt.CompareAndSwap(true, false) {
			record := httptest.NewRecorder()
			original.ServeHTTP(record, r)
			if record.Code != 200 {
				t.Errorf("server did not commit: %s", record.Body.String())
			}
			http.Error(w, "lost acknowledgement after commit", 503)
			return
		}
		original.ServeHTTP(w, r)
	})
	path, until := prepareMirror(t, a, b, data)
	args := []string{"run", "--state-dir", path, "--source", a.viewerFile, "--destination", b.operatorFile}
	first, e := mirrorCLI(t, args...)
	if e == nil || first.Phase != "blocked" || first.DestinationReceived != 0 {
		t.Fatal(first, e)
	}
	if _, e = b.blobs.Get(evidence.GetRequest{SHA256: first.Plan.Reference.SHA256, Length: 1}); e == nil {
		t.Fatal("partial replica appeared available")
	}
	mu.Lock()
	offsets = nil
	mu.Unlock()
	second, e := mirrorCLI(t, args...)
	if e != nil || second.Phase != "verified" || !second.DestinationComplete || second.Plan.RetainUntilUnixMS != until.UnixMilli() {
		t.Fatal(second, e)
	}
	mu.Lock()
	resumed := append([]int64{}, offsets...)
	mu.Unlock()
	if len(resumed) == 0 || resumed[0] != evidence.MaxChunk {
		t.Fatal("did not resume committed offset", resumed)
	}
	a.server.Close()
	verified, e := mirrorCLI(t, "verify", "--state-dir", path, "--destination", b.viewerFile)
	if e != nil || verified.Phase != "verified" || verified.CurrentVerification || verified.OriginalCaptureOwnerClaimed {
		t.Fatal(verified, e)
	}
	status, e := mirrorCLI(t, "status", "--state-dir", path)
	if e != nil || status.VerifiedUnixMS != verified.VerifiedUnixMS || status.Availability != "verified_at_last_check_current_availability_unknown" {
		t.Fatal(status, e)
	}
}
func TestMirrorRejectsChangedSourceExpiryAndWrongCredentialsBeforeAllocation(t *testing.T) {
	for _, kind := range []string{"expiry", "mime", "source-key", "destination-scope", "destination-write-audience", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			a, b := mirrorTestServer(t), mirrorTestServer(t)
			path, _ := prepareMirror(t, a, b, bytes.Repeat([]byte("synthetic bytes\n"), 9000))
			original := a.server.Config.Handler
			if kind == "expiry" || kind == "mime" {
				a.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					record := httptest.NewRecorder()
					original.ServeHTTP(record, r)
					var q evidence.GetResponse
					if record.Code == 200 && r.URL.Path == "/v1/evidence/get" && json.Unmarshal(record.Body.Bytes(), &q) == nil {
						if kind == "expiry" {
							q.RetainUntilUnixMS++
						} else {
							q.ContentType = "audio/wav"
						}
						json.NewEncoder(w).Encode(q)
						return
					}
					for k, v := range record.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(record.Code)
					w.Write(record.Body.Bytes())
				})
			}
			source := a.viewerFile
			destination := b.operatorFile
			if kind == "source-key" {
				source = a.operatorFile
			}
			if kind == "destination-scope" {
				changed := b.operator
				changed.Grant.Grant.Site = "other-site"
				writeSiteJSON(t, b.operatorFile, changed)
			}
			if kind == "destination-write-audience" {
				changed := b.operator
				g := changed.Grant.Grant
				g.PublishAudience = "other-node"
				var err error
				changed.Grant, err = authority.SignGrant(g, b.root)
				if err != nil {
					t.Fatal(err)
				}
				writeSiteJSON(t, b.operatorFile, changed)
			}
			if kind == "revoked" {
				rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: time.Now().Add(-time.Millisecond).UnixMilli(), ExpiresAtUnixMS: time.Now().Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{a.viewer.Grant.Grant.ID}}, a.root)
				if e != nil {
					t.Fatal(e)
				}
				if e = a.verifier.UpdateRevocations(rev); e != nil {
					t.Fatal(e)
				}
			}
			status, e := mirrorCLI(t, "run", "--state-dir", path, "--source", source, "--destination", destination)
			if e == nil {
				t.Fatal("invalid source/scope admitted")
			}
			names, _ := os.ReadDir(filepath.Join(b.dir, "blobs"))
			for _, f := range names {
				if f.Name() == status.Plan.Reference.SHA256+".json" {
					t.Fatal("destination allocated before source validity")
				}
			}
		})
	}
}
func TestMirrorDestinationQuotaAndOwnershipPreserveOtherEvidence(t *testing.T) {
	for _, kind := range []string{"quota", "owner"} {
		t.Run(kind, func(t *testing.T) {
			a, b := mirrorTestServer(t), mirrorTestServer(t)
			data := bytes.Repeat([]byte("selected evidence\n"), 10000)
			path, until := prepareMirror(t, a, b, data)
			j, e := evidencemirror.Open(path)
			if e != nil {
				t.Fatal(e)
			}
			plan := j.Snapshot().Plan
			j.Close()
			if kind == "quota" {
				b.blobs.Close()
				b.blobs, e = evidence.Open(filepath.Join(b.dir, "blobs"), evidence.Config{Tenant: "lab", Site: "synthetic-office", QuotaBytes: 200000, MaxBlobBytes: 200000})
				if e != nil {
					t.Fatal(e)
				}
				seedEvidence(t, b, bytes.Repeat([]byte("unrelated protected"), 5000), "image/png", until)
			} else {
				_, e = b.blobs.Put("another-owner", evidence.PutRequest{SHA256: plan.Reference.SHA256, Size: plan.Reference.Size, ContentType: plan.Reference.ContentType, RetainUntilUnixMS: until.UnixMilli()})
				if e != nil {
					t.Fatal(e)
				}
			}
			before, _ := os.ReadDir(filepath.Join(b.dir, "blobs"))
			_, e = mirrorCLI(t, "run", "--state-dir", path, "--source", a.viewerFile, "--destination", b.operatorFile)
			if e == nil {
				t.Fatal("quota/owner conflict accepted")
			}
			after, _ := os.ReadDir(filepath.Join(b.dir, "blobs"))
			if len(before) != len(after) {
				t.Fatal("failed mirror changed unrelated destination allocations")
			}
		})
	}
}

func TestMirrorCompletedCacheDoesNotAuthorizeAnotherWrite(t *testing.T) {
	a, b := mirrorTestServer(t), mirrorTestServer(t)
	path, _ := prepareMirror(t, a, b, bytes.Repeat([]byte("original retained bytes\n"), 8000))
	args := []string{"run", "--state-dir", path, "--source", a.viewerFile, "--destination", b.operatorFile}
	first, e := mirrorCLI(t, args...)
	if e != nil || first.Phase != "verified" {
		t.Fatal(first, e)
	}
	rev, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "lab", Site: "synthetic-office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: time.Now().Add(-time.Millisecond).UnixMilli(), ExpiresAtUnixMS: time.Now().Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{a.viewer.Grant.Grant.ID}}, a.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.verifier.UpdateRevocations(rev); e != nil {
		t.Fatal(e)
	}
	denied, e := mirrorCLI(t, args...)
	if e == nil || denied.LastError != "denied" || denied.VerifiedUnixMS != first.VerifiedUnixMS {
		t.Fatal("cache bypassed current access", denied, e)
	}
	verified, e := mirrorCLI(t, "verify", "--state-dir", path, "--destination", b.viewerFile)
	if e != nil || verified.Phase != "verified" {
		t.Fatal("independently authorized replica read failed", verified, e)
	}
}
func TestMirrorTamperedDestinationReceiptIsNeverVerified(t *testing.T) {
	a, b := mirrorTestServer(t), mirrorTestServer(t)
	path, _ := prepareMirror(t, a, b, bytes.Repeat([]byte("original media\n"), 8000))
	original := b.server.Config.Handler
	b.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr := httptest.NewRecorder()
		original.ServeHTTP(rr, r)
		if r.URL.Path == "/v1/evidence/put" && rr.Code == 200 {
			var reply evidence.PutResponse
			json.Unmarshal(rr.Body.Bytes(), &reply)
			reply.RetainUntilUnixMS++
			json.NewEncoder(w).Encode(reply)
			return
		}
		w.WriteHeader(rr.Code)
		w.Write(rr.Body.Bytes())
	})
	status, e := mirrorCLI(t, "run", "--state-dir", path, "--source", a.viewerFile, "--destination", b.operatorFile)
	if e == nil || status.Phase == "verified" || status.VerifiedUnixMS != 0 || status.DestinationComplete {
		t.Fatal(status, e)
	}
}
