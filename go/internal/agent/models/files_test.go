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
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestFileCacheFollowsOnlyHTTPSRedirects(t *testing.T) {
	body := []byte("model weights")
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(plain.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-https":
			http.Redirect(w, r, "/m", http.StatusFound)
		case "/to-http":
			http.Redirect(w, r, plain.URL+"/m", http.StatusFound)
		default:
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	f := File{SHA256: digest(body), Bytes: int64(len(body))}

	f.URL = srv.URL + "/to-https"
	if _, err := NewFileCache(t.TempDir(), srv.Client()).Fetch(context.Background(), f); err != nil {
		t.Fatalf("Fetch through an https redirect = %v", err)
	}
	f.URL = srv.URL + "/to-http"
	if _, err := NewFileCache(t.TempDir(), srv.Client()).Fetch(context.Background(), f); err == nil {
		t.Fatal("Fetch followed a redirect to plain http")
	}
	if plainHits.Load() != 0 {
		t.Fatalf("the plain http server was asked %d times", plainHits.Load())
	}
}

func TestFileCacheAbandonsAStalledDownload(t *testing.T) {
	body := []byte("model weights")
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body[:4])
		w.(http.Flusher).Flush()
		select { // never send the rest
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first, so Close does not wait on the handler
	cache := NewFileCache(t.TempDir(), srv.Client())
	cache.stall = 100 * time.Millisecond
	f := File{URL: srv.URL + "/m", SHA256: digest(body), Bytes: int64(len(body))}

	done := make(chan error, 1)
	go func() { _, err := cache.Fetch(context.Background(), f); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("Fetch = %v, want a stall error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch is still waiting on a server that stopped sending")
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
