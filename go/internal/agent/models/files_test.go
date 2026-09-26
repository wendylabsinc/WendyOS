package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serve(t *testing.T, body []byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFileCacheFetchesVerifiesAndReuses(t *testing.T) {
	body := []byte("model weights")
	var hits atomic.Int32
	srv := serve(t, body, &hits)
	cache := NewFileCache(t.TempDir(), srv.Client())
	f := File{URL: srv.URL + "/m", SHA256: digest(body), Bytes: int64(len(body))}

	path, err := cache.Fetch(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) || !cache.Has(f.SHA256) || path != cache.Path(f.SHA256) {
		t.Fatalf("cached %q at %s", got, path)
	}
	if _, err := cache.Fetch(context.Background(), f); err != nil || hits.Load() != 1 {
		t.Fatalf("second fetch: err=%v downloads=%d, want one download", err, hits.Load())
	}
}

func TestFileCacheRejectsTamperedAndOversizedBodies(t *testing.T) {
	good := []byte("model weights")
	cases := map[string][]byte{
		"tampered":  []byte("tampered data"), // same length, different digest
		"oversized": append(append([]byte{}, good...), '!'),
	}
	for name, served := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			srv := serve(t, served, nil)
			cache := NewFileCache(root, srv.Client())
			f := File{URL: srv.URL + "/m", SHA256: digest(good), Bytes: int64(len(good))}
			if _, err := cache.Fetch(context.Background(), f); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("Fetch = %v, want ErrDigestMismatch", err)
			}
			if cache.Has(f.SHA256) {
				t.Fatal("an unverified file was cached")
			}
			if left, _ := os.ReadDir(filepath.Join(root, "sha256")); len(left) != 0 {
				t.Fatalf("partial downloads left behind: %v", left)
			}
		})
	}
}

func TestFileCacheReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	cache := NewFileCache(t.TempDir(), srv.Client())
	_, err := cache.Fetch(context.Background(), File{URL: srv.URL + "/m", SHA256: digest([]byte("x")), Bytes: 1})
	if err == nil || errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Fetch = %v, want an HTTP error", err)
	}
}

func TestEngineCached(t *testing.T) {
	root := t.TempDir()
	sha := digest([]byte("m"))
	if engineCached(root, sha, "sm_87") {
		t.Fatal("found an engine in an empty cache")
	}
	if err := os.MkdirAll(filepath.Join(root, sha), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sha, "sm_87-trt10.7.plan"), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !engineCached(root, sha, "sm_87") || engineCached(root, sha, "sm_110") || engineCached(root, sha, "") {
		t.Fatal("engine lookup ignores the GPU architecture")
	}
}
