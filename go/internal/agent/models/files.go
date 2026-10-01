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
	"time"
)

// ErrDigestMismatch means a downloaded model file did not match the catalog.
var ErrDigestMismatch = errors.New("model file failed verification")

// FileCache keeps verified model files under root/sha256/<digest>. The
// content-addressed layout is the one WDY-3139 proposes for a device-side
// artifact store.
type FileCache struct {
	root   string
	client *http.Client
	stall  time.Duration // a download that receives nothing for this long fails

	mu    sync.Mutex
	locks map[string]*sync.Mutex // one per digest, so one download serves concurrent starts
}

const (
	maxModelFileRedirects = 5
	// downloadStall bounds silence, not the whole download, so a large file
	// on a slow link still arrives.
	downloadStall = time.Minute
)

var errDownloadStalled = errors.New("download stalled")

// NewFileCache stores files under root. A nil client uses the default
// transport. Whatever the client, a download follows only https redirects.
func NewFileCache(root string, client *http.Client) *FileCache {
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = httpsRedirectsOnly
	return &FileCache{root: root, client: &c, stall: downloadStall, locks: map[string]*sync.Mutex{}}
}

// httpsRedirectsOnly keeps a download on https: the catalog pins https URLs,
// and a redirect must not weaken that. The digest still decides what is kept.
func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
	}
	if len(via) >= maxModelFileRedirects {
		return fmt.Errorf("stopped after %d redirects", maxModelFileRedirects)
	}
	return nil
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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(c.stall, func() { cancel(errDownloadStalled) })
	defer timer.Stop()
	failed := func(err error) error {
		if context.Cause(ctx) == errDownloadStalled {
			return fmt.Errorf("downloading model file: no data for %s: %w", c.stall, errDownloadStalled)
		}
		return fmt.Errorf("downloading model file: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return failed(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading model file: %s", resp.Status)
	}
	h := sha256.New()
	// Read one byte past the declared size so an oversized body is caught.
	body := stallReader{r: io.LimitReader(resp.Body, f.Bytes+1), timer: timer, stall: c.stall}
	n, err := io.Copy(io.MultiWriter(dst, h), body)
	if err != nil {
		return failed(err)
	}
	if n != f.Bytes {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrDigestMismatch, n, f.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%w: sha256 %s, want %s", ErrDigestMismatch, got, f.SHA256)
	}
	return nil
}

// stallReader pushes the stall deadline back whenever data arrives.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
}

func (s stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.stall)
	}
	return n, err
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
