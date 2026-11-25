// Package packagecache retains bounded, verified download progress. Cached bytes
// are not authority to install, publish, execute, or refresh a signed document.
package packagecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

const DefaultMaxBytes int64 = 256 << 20
const DefaultMaxEntries = 1024
const maxBlobBytes int64 = 64 << 20

var ErrLocked = errors.New("package cache is in use")
var ErrQuota = errors.New("package cache quota exceeded; discard an unused entry")
var ErrFaulted = errors.New("package cache requires reopen after an uncertain write")

type Scope struct {
	Tenant   string `json:"tenant"`
	Site     string `json:"site"`
	Domain   string `json:"domain"`
	Audience string `json:"audience"`
	Endpoint string `json:"endpoint"`
}

type Config struct {
	// Evidence enables the bounded evidence MIME catalog; default package caches remain octet-stream only.
	Evidence   bool
	Scope      Scope
	MaxBytes   int64
	MaxEntries int
	Now        func() time.Time
}

type header struct {
	Evidence   bool  `json:"evidence,omitempty"`
	Version    int   `json:"version"`
	Scope      Scope `json:"scope"`
	MaxBytes   int64 `json:"max_bytes"`
	MaxEntries int   `json:"max_entries"`
}

type Entry struct {
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	Received          int64  `json:"received"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
	PrefixSHA256      string `json:"prefix_sha256"`
	Complete          bool   `json:"complete"`
}

type Inventory struct {
	Scope         Scope   `json:"scope"`
	MaxBytes      int64   `json:"max_bytes"`
	MaxEntries    int     `json:"max_entries"`
	ReservedBytes int64   `json:"reserved_bytes"`
	StoredBytes   int64   `json:"stored_bytes"`
	Entries       []Entry `json:"entries"`
}

type Progress struct {
	CacheHit        bool  `json:"cache_hit"`
	ResumedBytes    int64 `json:"resumed_bytes"`
	DownloadedBytes int64 `json:"downloaded_bytes"`
}

type Getter func(context.Context, evidence.GetRequest) (evidence.GetResponse, error)

type Cache struct {
	mu      sync.Mutex
	root    *os.Root
	lock    *os.File
	header  header
	now     func() time.Time
	entries map[string]Entry
	fault   error
	closed  bool
	// Test fault boundary; never supplied by a network response or CLI flag.
	afterWrite func(string) error
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == value
}

func private(info os.FileInfo, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 || info.IsDir() != directory || (!directory && (!info.Mode().IsRegular() || stat.Nlink != 1)) {
		return errors.New("cache paths must be private, owned directories or single-link regular files")
	}
	return nil
}

func openRoot(path string, create bool) (*os.Root, error) {
	if path == "" {
		return nil, errors.New("cache directory required")
	}
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = private(info, true); err != nil {
		return nil, err
	}
	return os.OpenRoot(path)
}

func read(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = private(info, false); err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, errors.New("cache file exceeds bound")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if err = private(info, false); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("cache read exceeds bound")
	}
	return b, err
}

// openWritable checks the descriptor used for mutation, after any network wait.
// A pathname check before that wait cannot reject a newly linked or replaced file.
func openWritable(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = private(info, false)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (c *Cache) syncRoot() error {
	f, err := c.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (c *Cache) boundary(stage string) error {
	if c.afterWrite != nil {
		return c.afterWrite(stage)
	}
	return nil
}

func (c *Cache) atomic(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if info, err := c.root.Lstat(name); err == nil {
		if err = private(info, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := c.root.OpenFile(".checkpoint", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer c.root.Remove(".checkpoint")
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = c.root.Rename(".checkpoint", name); err != nil {
		return err
	}
	if err = c.boundary("after-checkpoint-rename"); err != nil {
		return err
	}
	if err = c.syncRoot(); err != nil {
		return err
	}
	return c.boundary("after-checkpoint-sync")
}

func Open(path string, cfg Config) (*Cache, error) { return openCache(path, cfg, true) }

func openCache(path string, cfg Config, recoverEntries bool) (*Cache, error) {
	for _, v := range []string{cfg.Scope.Tenant, cfg.Scope.Site, cfg.Scope.Domain, cfg.Scope.Audience, cfg.Scope.Endpoint} {
		if v == "" || len(v) > 2048 {
			return nil, errors.New("explicit bounded cache scope required")
		}
	}
	if cfg.MaxBytes < 0 || cfg.MaxBytes > 1<<30 || cfg.MaxEntries < 0 || cfg.MaxEntries > 4096 {
		return nil, errors.New("invalid cache quota")
	}
	root, err := openRoot(path, true)
	if err != nil {
		return nil, err
	}
	return openCacheRoot(root, cfg, recoverEntries)
}

// OpenRoot takes ownership of an already anchored directory, including on failure.
func OpenRoot(root *os.Root, cfg Config) (*Cache, error) { return openCacheRoot(root, cfg, true) }

func openCacheRoot(root *os.Root, cfg Config, recoverEntries bool) (_ *Cache, resultErr error) {
	defer func() {
		if resultErr != nil {
			root.Close()
		}
	}()
	info, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if err = private(info, true); err != nil {
		return nil, err
	}
	for _, value := range []string{cfg.Scope.Tenant, cfg.Scope.Site, cfg.Scope.Domain, cfg.Scope.Audience, cfg.Scope.Endpoint} {
		if value == "" || len(value) > 2048 {
			return nil, errors.New("explicit bounded cache scope required")
		}
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.MaxBytes < 1 || cfg.MaxBytes > 1<<30 || cfg.MaxEntries < 1 || cfg.MaxEntries > 4096 {
		return nil, errors.New("invalid cache quota")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &Cache{root: root, header: header{Version: 1, Scope: cfg.Scope, MaxBytes: cfg.MaxBytes, MaxEntries: cfg.MaxEntries, Evidence: cfg.Evidence}, now: cfg.Now, entries: make(map[string]Entry)}
	defer func() {
		if resultErr != nil {
			c.Close()
		}
	}()
	c.lock, err = root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err = c.lock.Stat()
	if err != nil {
		return nil, err
	}
	if err = private(info, false); err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(c.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, ErrLocked
	}
	raw, err := read(root, "scope.json", 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		// An unbound directory must be empty apart from our lock.
		f, e := root.Open(".")
		if e != nil {
			return nil, e
		}
		names, e := f.Readdirnames(3)
		f.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return nil, e
		}
		if len(names) != 1 || names[0] != ".lock" {
			return nil, errors.New("refuse to adopt a nonempty unbound cache")
		}
		if err = c.atomic("scope.json", c.header); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		var saved header
		if err = authority.DecodeStrictLimit(raw, &saved, 16<<10); err != nil {
			return nil, err
		}
		if saved != c.header {
			return nil, errors.New("cache scope or quota differs from requested configuration")
		}
	}
	// Only a validated, bound cache may have interrupted metadata removed.
	if info, err := root.Lstat(".checkpoint"); err == nil {
		if err = private(info, false); err != nil {
			return nil, err
		}
		if err = root.Remove(".checkpoint"); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if recoverEntries {
		if err = c.recover(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Cache) recover() error {
	f, err := c.root.Open(".")
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(8195)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) >= 8195 {
		return errors.New("cache directory exceeds entry bound")
	}
	var dataFiles []string
	var reserved int64
	for _, name := range names {
		if name == ".lock" || name == "scope.json" {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".data")
		if !validDigest(base) || (name != base+".json" && name != base+".data") {
			return errors.New("unexpected file in cache")
		}
		if strings.HasSuffix(name, ".data") {
			dataFiles = append(dataFiles, name)
			continue
		}
		raw, err := read(c.root, name, 16<<10)
		if err != nil {
			return err
		}
		var entry Entry
		if err = authority.DecodeStrictLimit(raw, &entry, 16<<10); err != nil {
			return err
		}
		if entry.SHA256 != base || entry.Size < 1 || entry.Size > maxBlobBytes || entry.Received < 0 || entry.Received > entry.Size || entry.Complete != (entry.Received == entry.Size) || !validDigest(entry.PrefixSHA256) || !c.accepts(entry.ContentType) || entry.RetainUntilUnixMS <= 0 {
			return errors.New("invalid cache checkpoint")
		}
		c.entries[base] = entry
		reserved += entry.Size
		if len(c.entries) > c.header.MaxEntries || reserved > c.header.MaxBytes {
			return ErrQuota
		}
		if _, err = c.verifiedPrefix(entry, true); err != nil {
			return err
		}
	}
	for _, name := range dataFiles {
		if _, ok := c.entries[strings.TrimSuffix(name, ".data")]; ok {
			continue
		}
		info, err := c.root.Lstat(name)
		if err != nil {
			return err
		}
		if err = private(info, false); err != nil {
			return err
		}
		if err = c.root.Remove(name); err != nil {
			return err
		}
	}
	return c.syncRoot()
}

func (c *Cache) verifiedPrefix(e Entry, truncate bool) ([]byte, error) {
	data, err := read(c.root, e.SHA256+".data", e.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) < e.Received {
		return nil, errors.New("cache checkpoint exceeds durable bytes")
	}
	data = data[:e.Received]
	if digest(data) != e.PrefixSHA256 || (e.Complete && digest(data) != e.SHA256) {
		return nil, errors.New("cache data failed integrity verification")
	}
	if truncate {
		f, err := openWritable(c.root, e.SHA256+".data", os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		err = f.Truncate(e.Received)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return data, nil
}

func (c *Cache) check() error {
	if c.closed {
		return errors.New("cache is closed")
	}
	if c.fault != nil {
		return ErrFaulted
	}
	return nil
}
func (c *Cache) failed(err error) error {
	if err != nil {
		c.fault = err
	}
	return err
}

// Fetch reauthorizes every invocation through get, including a one-byte probe
// on a complete cache hit. It never follows a server-supplied URL. Installation
// must independently recheck the signed site publication after this returns.
func (c *Cache) Fetch(ctx context.Context, hash string, maxBytes int64, get Getter) ([]byte, Progress, error) {
	return c.fetch(ctx, hash, maxBytes, "application/octet-stream", false, get)
}

// FetchEvidence binds every response and checkpoint to the exact reference.
func (c *Cache) FetchEvidence(ctx context.Context, hash string, size int64, contentType string, get Getter) ([]byte, Progress, error) {
	if !c.header.Evidence {
		return nil, Progress{}, errors.New("evidence cache mode required")
	}
	return c.fetch(ctx, hash, size, contentType, true, get)
}

func ValidContentType(t string) bool {
	switch t {
	case "application/octet-stream", "application/json", "image/png", "image/jpeg", "audio/wav", "video/mp4":
		return true
	}
	return false
}
func (c *Cache) accepts(t string) bool {
	return t == "application/octet-stream" || c.header.Evidence && ValidContentType(t)
}

func (c *Cache) fetch(ctx context.Context, hash string, maxBytes int64, contentType string, exact bool, get Getter) ([]byte, Progress, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var progress Progress
	if err := c.check(); err != nil {
		return nil, progress, err
	}
	if !validDigest(hash) || maxBytes < 1 || maxBytes > maxBlobBytes || get == nil || !c.accepts(contentType) {
		return nil, progress, errors.New("invalid bounded cache fetch")
	}
	e, exists := c.entries[hash]
	var data []byte
	var err error
	if exists {
		if e.Size > maxBytes || e.ContentType != contentType || exact && e.Size != maxBytes {
			return nil, progress, errors.New("cached blob exceeds requested bound")
		}
		data, err = c.verifiedPrefix(e, false)
		if err != nil {
			return nil, progress, err
		}
		if !e.Complete {
			progress.ResumedBytes = e.Received
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, progress, err
		}
		offset, length := e.Received, evidence.MaxChunk
		if e.Complete {
			offset, length = 0, 1
		}
		r, err := get(ctx, evidence.GetRequest{SHA256: hash, Offset: offset, Length: length})
		if err != nil {
			return nil, progress, err
		}
		if err = ctx.Err(); err != nil {
			return nil, progress, err
		}
		if r.SHA256 != hash || r.Offset != offset || r.Size < 1 || r.Size > maxBytes || len(r.Data) < 1 || len(r.Data) > length || int64(len(r.Data)) > r.Size-offset || r.ContentType != contentType || exact && r.Size != maxBytes || r.RetainUntilUnixMS <= c.now().UnixMilli() {
			return nil, progress, errors.New("invalid or expired evidence chunk")
		}
		if exists && (r.Size != e.Size || r.ContentType != e.ContentType || r.RetainUntilUnixMS != e.RetainUntilUnixMS) {
			return nil, progress, errors.New("cached evidence metadata changed; discard the entry before retrying")
		}
		progress.DownloadedBytes += int64(len(r.Data))
		if e.Complete {
			if r.Data[0] != data[0] {
				return nil, progress, errors.New("evidence probe differs from verified cached bytes")
			}
			progress.CacheHit = true
			return data, progress, nil
		}
		if !exists {
			var reserved int64
			for _, stored := range c.entries {
				reserved += stored.Size
			}
			if len(c.entries) >= c.header.MaxEntries || r.Size > c.header.MaxBytes-reserved {
				return nil, progress, ErrQuota
			}
			e = Entry{SHA256: hash, Size: r.Size, ContentType: r.ContentType, RetainUntilUnixMS: r.RetainUntilUnixMS}
		}
		flags := os.O_WRONLY
		if !exists {
			flags |= os.O_CREATE | os.O_EXCL
		}
		f, err := openWritable(c.root, hash+".data", flags, 0600)
		if err != nil {
			return nil, progress, c.failed(err)
		}
		_, err = f.WriteAt(r.Data, e.Received)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = c.boundary("after-data-sync")
		}
		if err != nil {
			return nil, progress, c.failed(err)
		}
		data = append(data, r.Data...)
		e.Received = int64(len(data))
		e.PrefixSHA256 = digest(data)
		e.Complete = e.Received == e.Size
		if e.Complete && e.PrefixSHA256 != hash {
			return nil, progress, c.failed(errors.New("download digest mismatch"))
		}
		if err = c.atomic(hash+".json", e); err != nil {
			return nil, progress, c.failed(err)
		}
		c.entries[hash] = e
		exists = true
		if e.Complete {
			if e.RetainUntilUnixMS <= c.now().UnixMilli() {
				return nil, progress, errors.New("evidence expired during persistence")
			}
			return data, progress, nil
		}
	}
}

func (c *Cache) Inventory() (Inventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return Inventory{}, err
	}
	out := Inventory{Scope: c.header.Scope, MaxBytes: c.header.MaxBytes, MaxEntries: c.header.MaxEntries, Entries: []Entry{}}
	for _, entry := range c.entries {
		out.Entries = append(out.Entries, entry)
		out.ReservedBytes += entry.Size
		out.StoredBytes += entry.Received
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].SHA256 < out.Entries[j].SHA256 })
	return out, nil
}

func (c *Cache) Discard(hash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.check(); err != nil {
		return err
	}
	if !validDigest(hash) {
		return errors.New("invalid cache digest")
	}
	if _, ok := c.entries[hash]; !ok {
		return os.ErrNotExist
	}
	return c.discardFiles(hash)
}

func (c *Cache) discardFiles(hash string) error {
	if !validDigest(hash) {
		return errors.New("invalid cache digest")
	}
	found := false
	for _, suffix := range []string{".json", ".data"} {
		info, err := c.root.Lstat(hash + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = private(info, false); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return os.ErrNotExist
	}
	// Remove the checkpoint first; recovery safely discards any orphaned bytes.
	if err := c.root.Remove(hash + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.failed(err)
	}
	if err := c.syncRoot(); err != nil {
		return c.failed(err)
	}
	if err := c.root.Remove(hash + ".data"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.failed(err)
	}
	if err := c.syncRoot(); err != nil {
		return c.failed(err)
	}
	delete(c.entries, hash)
	return nil
}

func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.lock != nil {
		syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
		c.lock.Close()
	}
	if c.root != nil {
		return c.root.Close()
	}
	return nil
}

func existing(path string, recoverEntries bool) (*Cache, error) {
	root, err := openRoot(path, false)
	if err != nil {
		return nil, err
	}
	return existingRoot(root, recoverEntries)
}
func existingRoot(root *os.Root, recoverEntries bool) (*Cache, error) {
	raw, err := read(root, "scope.json", 16<<10)
	if err != nil {
		root.Close()
		return nil, err
	}
	var h header
	if err = authority.DecodeStrictLimit(raw, &h, 16<<10); err != nil {
		root.Close()
		return nil, err
	}
	if h.Version != 1 {
		root.Close()
		return nil, fmt.Errorf("unsupported cache version %d", h.Version)
	}
	return openCacheRoot(root, Config{Scope: h.Scope, MaxBytes: h.MaxBytes, MaxEntries: h.MaxEntries, Evidence: h.Evidence}, recoverEntries)
}

// InspectRoot and DiscardRoot take ownership of the directory descriptor.
func InspectRoot(root *os.Root) (Inventory, error) {
	c, err := existingRoot(root, true)
	if err != nil {
		return Inventory{}, err
	}
	defer c.Close()
	return c.Inventory()
}
func DiscardRoot(root *os.Root, hash string) error {
	c, err := existingRoot(root, false)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.discardFiles(hash)
}

func Inspect(path string) (Inventory, error) {
	c, err := existing(path, true)
	if err != nil {
		return Inventory{}, err
	}
	defer c.Close()
	return c.Inventory()
}
func Discard(path, hash string) error {
	// Explicit discard must remain usable when the requested checkpoint or blob
	// is corrupt. It still locks and validates the root/scope and both exact paths.
	c, err := existing(path, false)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.discardFiles(hash)
}
