package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// ErrDigestMismatch means a downloaded model file did not match the catalog.
var ErrDigestMismatch = errors.New("model file failed verification")

// FileCache keeps verified model files under root/sha256/<digest>. The
// content-addressed layout is the one WDY-3139 proposes for a device-side
// artifact store.
type FileCache struct {
	root   string
	client *http.Client

	mu    sync.Mutex
	locks map[string]*sync.Mutex // one per digest, so one download serves concurrent starts
}

// NewFileCache stores files under root. A nil client uses http.DefaultClient.
func NewFileCache(root string, client *http.Client) *FileCache {
	if client == nil {
		client = http.DefaultClient
	}
	return &FileCache{root: root, client: client, locks: map[string]*sync.Mutex{}}
}

// Path is where the file with this digest lives once fetched.
func (c *FileCache) Path(sha string) string { return filepath.Join(c.root, "sha256", sha) }

// Has reports whether a verified copy is already cached.
func (c *FileCache) Has(sha string) bool {
	info, err := os.Stat(c.Path(sha))
	return err == nil && info.Mode().IsRegular()
}

// Fetch returns the path of a verified copy of f, downloading it first when
// needed. A download is renamed into place only after its size and digest
// match, so every cached path is verified.
func (c *FileCache) Fetch(ctx context.Context, f File) (string, error) {
	lock := c.lock(f.SHA256)
	lock.Lock()
	defer lock.Unlock()
	path := c.Path(f.SHA256)
	if c.Has(f.SHA256) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename
	defer tmp.Close()
	if err := c.download(ctx, f, tmp); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func (c *FileCache) download(ctx context.Context, f File, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading model file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading model file: %s", resp.Status)
	}
	h := sha256.New()
	// Read one byte past the declared size so an oversized body is caught.
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, f.Bytes+1))
	if err != nil {
		return fmt.Errorf("downloading model file: %w", err)
	}
	if n != f.Bytes {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrDigestMismatch, n, f.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%w: sha256 %s, want %s", ErrDigestMismatch, got, f.SHA256)
	}
	return nil
}

func (c *FileCache) lock(sha string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[sha]
	if !ok {
		l = &sync.Mutex{}
		c.locks[sha] = l
	}
	return l
}

// engineCached reports whether a TensorRT engine built from fileSHA for this
// GPU architecture exists. Hosts name engines <gpu_arch>-trt<version>.plan and
// record their provenance beside them (design §6.3).
func engineCached(enginesRoot, fileSHA, gpuArch string) bool {
	if gpuArch == "" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(enginesRoot, fileSHA, gpuArch+"-trt*.plan"))
	return len(matches) > 0
}
