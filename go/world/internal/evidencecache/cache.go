// Package evidencecache groups independent, endpoint-bound download checkpoints.
// A cache contains bytes and progress, never permission to read or export them.
package evidencecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

const DefaultMaxBytes int64 = 256 << 20

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type Source struct {
	ID    string             `json:"id"`
	Scope packagecache.Scope `json:"scope"`
}
type Config struct {
	Sources  []Source `json:"sources"`
	MaxBytes int64    `json:"max_bytes"`
}
type catalog struct {
	Version int `json:"version"`
	Config
}
type Inventory struct {
	MaxBytes      int64             `json:"max_bytes"`
	ReservedBytes int64             `json:"reserved_bytes"`
	StoredBytes   int64             `json:"stored_bytes"`
	Sources       []SourceInventory `json:"sources"`
}
type SourceInventory struct {
	ID string `json:"id"`
	packagecache.Inventory
}
type Cache struct {
	root *os.Root
	lock *os.File
	cfg  Config
}

func normalize(c Config) (Config, error) {
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if len(c.Sources) < 1 || len(c.Sources) > 8 || c.MaxBytes < int64(len(c.Sources)) || c.MaxBytes > 1<<30 {
		return c, errors.New("evidence cache needs 1–8 sources and a bounded total byte quota")
	}
	c.Sources = append([]Source(nil), c.Sources...)
	sort.Slice(c.Sources, func(i, j int) bool { return c.Sources[i].ID < c.Sources[j].ID })
	scope := c.Sources[0].Scope
	endpoints := map[string]bool{}
	for i, s := range c.Sources {
		if !validID.MatchString(s.ID) || i > 0 && s.ID == c.Sources[i-1].ID || s.Scope.Tenant != scope.Tenant || s.Scope.Site != scope.Site || s.Scope.Domain != "simulation" {
			return c, errors.New("distinct source IDs and one tenant/site/simulation scope required")
		}
		for _, v := range []string{s.Scope.Tenant, s.Scope.Site, s.Scope.Audience, s.Scope.Endpoint} {
			if v == "" || len(v) > 2048 {
				return c, errors.New("invalid evidence source scope")
			}
		}
		key := s.Scope.Endpoint + "\n" + s.Scope.Audience
		if endpoints[key] {
			return c, errors.New("duplicate evidence endpoint/audience")
		}
		endpoints[key] = true
	}
	return c, nil
}
func check(info os.FileInfo, dir bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 || info.IsDir() != dir || !dir && (!info.Mode().IsRegular() || st.Nlink != 1) {
		return errors.New("private owned directories and singly linked regular files required")
	}
	return nil
}
func read(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = check(info, false); err != nil {
		return nil, err
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
	if err = check(info, false); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("cache catalog exceeds bound")
	}
	return b, err
}
func syncRoot(root *os.Root) error {
	f, e := root.Open(".")
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func open(path string, create bool) (*Cache, error) {
	if path == "" {
		return nil, errors.New("cache directory required")
	}
	if create {
		if e := os.MkdirAll(path, 0700); e != nil {
			return nil, e
		}
	}
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if e = check(info, true); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(path)
	if e != nil {
		return nil, e
	}
	c := &Cache{root: root}
	info, e = root.Stat(".")
	if e == nil {
		e = check(info, true)
	}
	if e != nil {
		c.Close()
		return nil, e
	}
	c.lock, e = root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		c.Close()
		return nil, e
	}
	info, e = c.lock.Stat()
	if e == nil {
		e = check(info, false)
	}
	if e == nil {
		e = syscall.Flock(int(c.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if e != nil {
		c.Close()
		return nil, errors.New("evidence cache is unsafe or in use")
	}
	return c, nil
}
func Open(path string, cfg Config) (*Cache, error) {
	cfg, err := normalize(cfg)
	if err != nil {
		return nil, err
	}
	c, err := open(path, true)
	if err != nil {
		return nil, err
	}
	if err = c.bind(cfg); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *Cache) bind(cfg Config) error {
	raw, err := read(c.root, "catalog.json", 32<<10)
	if errors.Is(err, os.ErrNotExist) {
		f, e := c.root.Open(".")
		if e != nil {
			return e
		}
		names, e := f.Readdirnames(3)
		f.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(names) != 1 || names[0] != ".lock" {
			return errors.New("refuse nonempty unbound evidence cache")
		}
		raw, e = json.Marshal(catalog{1, cfg})
		if e != nil {
			return e
		}
		f, e = c.root.OpenFile(".catalog.tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer c.root.Remove(".catalog.tmp")
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
		if e = c.root.Rename(".catalog.tmp", "catalog.json"); e != nil {
			return e
		}
		if e = syncRoot(c.root); e != nil {
			return e
		}
	} else if err != nil {
		return err
	}
	var saved catalog
	if err = authority.DecodeStrictLimit(raw, &saved, 32<<10); err != nil {
		return err
	}
	if saved.Version != 1 || !reflect.DeepEqual(saved.Config, cfg) {
		return errors.New("cache source catalog or quota changed; use a separate cache")
	}
	c.cfg = cfg
	return nil
}
func existing(path string) (*Cache, error) {
	c, e := open(path, false)
	if e != nil {
		return nil, e
	}
	raw, e := read(c.root, "catalog.json", 32<<10)
	if e != nil {
		c.Close()
		return nil, e
	}
	var v catalog
	e = authority.DecodeStrictLimit(raw, &v, 32<<10)
	if e != nil {
		c.Close()
		return nil, e
	}
	n, e := normalize(v.Config)
	if e == nil && (v.Version != 1 || !reflect.DeepEqual(n, v.Config)) {
		e = errors.New("invalid evidence cache catalog")
	}
	if e != nil {
		c.Close()
		return nil, e
	}
	c.cfg = n
	return c, nil
}
func (c *Cache) source(id string) (Source, error) {
	for _, s := range c.cfg.Sources {
		if s.ID == id {
			return s, nil
		}
	}
	return Source{}, errors.New("source is not in the cache catalog")
}
func (c *Cache) directory(id string, create bool) (*os.Root, error) {
	if _, err := c.source(id); err != nil {
		return nil, err
	}
	if create {
		if err := c.root.Mkdir(id, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncRoot(c.root); err != nil {
			return nil, err
		}
	}
	info, err := c.root.Lstat(id)
	if err != nil {
		return nil, err
	}
	if err = check(info, true); err != nil {
		return nil, err
	}
	return c.root.OpenRoot(id)
}
func (c *Cache) Source(id string) (*packagecache.Cache, error) {
	s, err := c.source(id)
	if err != nil {
		return nil, err
	}
	root, err := c.directory(id, true)
	if err != nil {
		return nil, err
	}
	return packagecache.OpenRoot(root, packagecache.Config{Scope: s.Scope, Evidence: true, MaxBytes: c.cfg.MaxBytes / int64(len(c.cfg.Sources)), MaxEntries: 128})
}
func (c *Cache) Close() error {
	if c.lock != nil {
		syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
		c.lock.Close()
		c.lock = nil
	}
	if c.root != nil {
		err := c.root.Close()
		c.root = nil
		return err
	}
	return nil
}
func Inspect(path string) (Inventory, error) {
	c, err := existing(path)
	if err != nil {
		return Inventory{}, err
	}
	defer c.Close()
	out := Inventory{MaxBytes: c.cfg.MaxBytes, Sources: []SourceInventory{}}
	for _, s := range c.cfg.Sources {
		root, e := c.directory(s.ID, false)
		v := packagecache.Inventory{Scope: s.Scope, MaxBytes: c.cfg.MaxBytes / int64(len(c.cfg.Sources)), MaxEntries: 128, Entries: []packagecache.Entry{}}
		if !errors.Is(e, os.ErrNotExist) {
			if e != nil {
				return out, e
			}
			v, e = packagecache.InspectRoot(root)
			if e != nil {
				return out, fmt.Errorf("source %s cache invalid", s.ID)
			}
			if v.Scope != s.Scope {
				return out, errors.New("source cache scope mismatch")
			}
		}
		out.Sources = append(out.Sources, SourceInventory{s.ID, v})
		out.ReservedBytes += v.ReservedBytes
		out.StoredBytes += v.StoredBytes
	}
	return out, nil
}
func Discard(path, id, hash string) error {
	c, err := existing(path)
	if err != nil {
		return err
	}
	defer c.Close()
	root, err := c.directory(id, false)
	if err != nil {
		return err
	}
	return packagecache.DiscardRoot(root, hash)
}
