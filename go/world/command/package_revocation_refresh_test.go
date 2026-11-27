package command

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func TestPackageTrustRefresh(t *testing.T) {
	for _, kind := range []string{"revoked-origin", "replacement-root", "rollback", "invalid-json", "missing-file"} {
		t.Run(kind, func(t *testing.T) {
			p := newPackageTestServer(t)
			publication := publishPackageTest(t, p)
			old := p.trust.Revocations
			revision := old.Revocations
			revision.Revision++
			signed, err := authority.SignRevocations(revision, p.root)
			if err != nil {
				t.Fatal(err)
			}
			p.trust.Revocations = signed
			writeSiteJSON(t, p.trustFile, p.trust)
			original := p.server.Config.Handler
			var once sync.Once
			p.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/evidence/get" {
					once.Do(func() {
						trust := p.trust
						rev := trust.Revocations.Revocations
						rev.Revision++
						key := p.root
						switch kind {
						case "revoked-origin":
							rev.GrantIDs = []string{p.operator.Grant.Grant.ID}
						case "replacement-root":
							pub, other, err := ed25519.GenerateKey(rand.Reader)
							if err != nil {
								t.Error(err)
								return
							}
							trust.RootPublicKey = pub
							key = other
						case "rollback":
							trust.Revocations = old
							writeSiteJSON(t, p.trustFile, trust)
							return
						case "invalid-json":
							if err := os.WriteFile(p.trustFile, []byte("invalid"), 0600); err != nil {
								t.Error(err)
							}
							return
						case "missing-file":
							if err := os.Remove(p.trustFile); err != nil {
								t.Error(err)
							}
							return
						}
						signed, err := authority.SignRevocations(rev, key)
						if err != nil {
							t.Error(err)
							return
						}
						trust.Revocations = signed
						writeSiteJSON(t, p.trustFile, trust)
					})
				}
				original.ServeHTTP(w, r)
			})
			var out bytes.Buffer
			target := filepath.Join(p.dir, "installed")
			err = runPackage([]string{"fetch", "--credentials", p.viewerFile, "--trust", p.trustFile, "--sha256", publication.SHA256, "--cache-dir", filepath.Join(p.dir, "cache"), "--out", target}, &out)
			if err == nil || out.Len() != 0 {
				t.Fatalf("installed despite trust change during transfer: %s %v", out.String(), err)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unauthorized destination exists", err)
			}
		})
	}
}

func TestPackageRootPin(t *testing.T) {
	p := newPackageTestServer(t)
	verifier, err := siteVerifier(p.trustFile, p.viewer)
	if err != nil {
		t.Fatal(err)
	}
	pub, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := p.trust
	trust.RootPublicKey = pub
	rev := trust.Revocations.Revocations
	rev.Revision++
	trust.Revocations, err = authority.SignRevocations(rev, other)
	if err != nil {
		t.Fatal(err)
	}
	writeSiteJSON(t, p.trustFile, trust)
	if err = refreshPackageRevocations(p.trustFile, verifier); err == nil {
		t.Fatal("replacement file rotated pinned root")
	}
	// A failed refresh has not replaced the original root or checkpoint.
	writeSiteJSON(t, p.trustFile, p.trust)
	if err = refreshPackageRevocations(p.trustFile, verifier); err != nil {
		t.Fatal(err)
	}
}
