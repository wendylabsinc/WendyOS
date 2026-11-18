package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type recordingCacheScope struct{}
type recordingCacheRoot struct{}

var recordingCacheMu sync.Mutex

const recordingCacheBytes int64 = 256 << 20

// Cache identity includes the authenticated account and device, never just a
// filename. A missing account disables disk caching rather than sharing scope.
func withRecordingCache(ctx context.Context, selector string) context.Context {
	cfg, err := config.Load()
	if err != nil {
		return ctx
	}
	auth, ok := cfg.ContextByName(cfg.CurrentContext)
	if !ok || len(auth.Certificates) == 0 {
		return ctx
	}
	identity := auth.Name + "\x00" + auth.CloudGRPC
	for _, cert := range auth.Certificates {
		identity += "\x00" + cert.PrincipalURI + "\x00" + cert.UserID
	}
	return context.WithValue(ctx, recordingCacheScope{}, identity+"\x00"+selector)
}
func recordingCachePath(ctx context.Context, file data.File) string {
	scope, _ := ctx.Value(recordingCacheScope{}).(string)
	if scope == "" {
		return ""
	}
	root, _ := ctx.Value(recordingCacheRoot{}).(string)
	if root == "" {
		var err error
		root, err = os.UserCacheDir()
		if err != nil {
			return ""
		}
	}
	key := sha256.Sum256([]byte(scope + "\x00" + file.SHA256))
	return filepath.Join(root, "WendyRecordingMedia-v1", hex.EncodeToString(key[:]))
}
func cachedRecordingFile(ctx context.Context, file data.File, target io.Writer) (bool, error) {
	name := recordingCachePath(ctx, file)
	if name == "" {
		return false, nil
	}
	recordingCacheMu.Lock()
	defer recordingCacheMu.Unlock()
	f, err := os.Open(name)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != file.Size {
		os.Remove(name)
		return false, nil
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != file.SHA256 {
		os.Remove(name)
		return false, nil
	}
	if err = ctx.Err(); err != nil {
		return true, err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return true, err
	}
	_, err = io.Copy(target, f)
	os.Chtimes(name, time.Now(), time.Now())
	return true, err
}
func cacheRecordingDownload(ctx context.Context, file data.File, target io.Writer, download func(io.Writer) error) error {
	name := recordingCachePath(ctx, file)
	if name == "" || file.Size > recordingCacheBytes {
		return download(target)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return download(target)
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".download-")
	if err != nil {
		return download(target)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	mirror := &optionalCacheWriter{target: target, cache: f}
	if err = download(mirror); err != nil {
		return err
	}
	if err = f.Close(); err != nil || mirror.failed {
		return nil
	} // cache persistence is optional
	recordingCacheMu.Lock()
	defer recordingCacheMu.Unlock()
	if os.Rename(f.Name(), name) != nil {
		return nil
	}
	entries, _ := os.ReadDir(filepath.Dir(name))
	type entry struct {
		name string
		info os.FileInfo
	}
	var files []entry
	for _, e := range entries {
		if e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		if info, e2 := e.Info(); e2 == nil {
			files = append(files, entry{e.Name(), info})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	var size int64
	for i, e := range files {
		size += e.info.Size()
		if i >= 80 || size > recordingCacheBytes {
			os.Remove(filepath.Join(filepath.Dir(name), e.name))
		}
	}
	return nil
}

// Disk exhaustion must never interrupt a successful network download.
type optionalCacheWriter struct {
	target, cache io.Writer
	failed        bool
}

func (w *optionalCacheWriter) Write(p []byte) (int, error) {
	n, err := w.target.Write(p)
	if !w.failed && n > 0 {
		written, e := w.cache.Write(p[:n])
		w.failed = e != nil || written != n
	}
	return n, err
}
