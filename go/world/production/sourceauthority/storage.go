package sourceauthority

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

type marker struct {
	Version                             int
	InstallationID, ConfigurationSHA256 string
}
type disk struct {
	Version  int
	Marker   marker
	Physical trustedtime.Interval
	Sources  map[string]State
}
type checked struct {
	SHA256 string
	Data   []byte
}
type Store struct {
	mu              sync.Mutex
	cfg             Config
	db              *bolt.DB
	state           disk
	observed        trustedtime.Interval
	closed, faulted bool
	commit          func(func(*bolt.Tx) error) error // private failure-injection seam
}

var bucket = []byte("source-authority-v1")
var stateKey = []byte("state")

func normalize(cfg Config) (Config, error) {
	if cfg.Clock == nil || cfg.Certificates == nil || cfg.AuthorizeLease == nil || cfg.AuthorizeFloor == nil || cfg.MaxSources < 1 || cfg.MaxSources > 256 || cfg.MaxBytes < MaxEnvelopeBytes || cfg.MaxBytes > 16<<20 || !domain(cfg.ExecutionDomain, cfg.SampleClockDomain) || !bounded(cfg.Site) {
		return cfg, ErrDenied
	}
	probe := Source{cfg.Tenant, "spiffe://wendy.sh/tenant/" + cfg.Tenant + "/device/22222222-2222-4222-8222-222222222222", "authority", "authority"}
	if probe.Validate() != nil {
		return cfg, ErrDenied
	}
	if !filepath.IsAbs(cfg.Path) || filepath.Clean(cfg.Path) != cfg.Path {
		return cfg, ErrDenied
	}
	return cfg, nil
}
func configDigest(c Config) string {
	b, _ := json.Marshal(struct {
		Tenant, Site, Execution, Sample string
		Sources, Bytes                  int
	}{c.Tenant, c.Site, c.ExecutionDomain, c.SampleClockDomain, c.MaxSources, c.MaxBytes})
	return hash(b)
}
func privateFile(path string) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != uint32(os.Geteuid()) || sys.Nlink != 1 || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() == 0 {
		return ErrCorrupt
	}
	return nil
}
func syncParent(path string) error {
	f, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func openDB(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second, OpenFile: func(p string, flags int, mode os.FileMode) (*os.File, error) {
		return os.OpenFile(p, (flags&^os.O_CREATE)|syscall.O_NOFOLLOW, mode)
	}})
}
func writeNew(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func storeBytes(d disk, limit int) ([]byte, error) {
	b, e := encode(d, limit)
	if e != nil {
		return nil, e
	}
	return json.Marshal(checked{hash(b), b})
}

// Create is explicit commissioning, never recovery. Interrupted creation leaves
// its marker in place and requires operator recovery; no old history is erased.
func Create(cfg Config) (s *Store, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if err = hostpath.EnsureDirectory(filepath.Dir(cfg.Path), 0700, true); err != nil {
		return nil, err
	}
	if _, e := os.Lstat(cfg.Path); e == nil || !errors.Is(e, os.ErrNotExist) {
		return nil, ErrCorrupt
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	m := marker{Version, hex.EncodeToString(nonce[:]), configDigest(cfg)}
	b, _ := json.Marshal(m)
	if err = writeNew(cfg.Path+".installation", b); err != nil {
		return nil, err
	}
	if err = syncParent(cfg.Path); err != nil {
		return nil, err
	}
	if err = writeNew(cfg.Path, nil); err != nil {
		return nil, err
	}
	db, err := openDB(cfg.Path)
	if err != nil {
		return nil, err
	}
	s = &Store{cfg: cfg, db: db, commit: db.Update, state: disk{Version: Version, Marker: m, Sources: map[string]State{}}}
	defer func() {
		if err != nil {
			db.Close()
			s = nil
		}
	}()
	w, err := s.now(context.Background())
	if err != nil {
		return nil, err
	}
	s.state.Physical = w
	b, err = storeBytes(s.state, cfg.MaxBytes)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bk, e := tx.CreateBucket(bucket)
		if e != nil {
			return e
		}
		return bk.Put(stateKey, b)
	})
	if err != nil {
		return nil, err
	}
	if err = syncParent(cfg.Path); err != nil {
		return nil, err
	}
	if _, err = s.now(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// Open restores historical counters/floors, including expired signed denials.
// The marker/checksum protects trusted local storage against accidents, not an
// adversary rolling back the entire volume. There is no implicit recreation.
func Open(cfg Config) (s *Store, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if err = hostpath.EnsureDirectory(filepath.Dir(cfg.Path), 0700, false); err != nil {
		return nil, err
	}
	for _, p := range []string{cfg.Path, cfg.Path + ".installation"} {
		if err = privateFile(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNotInitialized
			}
			return nil, err
		}
	}
	f, err := os.OpenFile(cfg.Path+".installation", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 2049))
	f.Close()
	if err != nil {
		return nil, err
	}
	var m marker
	if canonical(b, &m, 2048) != nil || m.Version != Version || !digestValid(m.InstallationID) || m.ConfigurationSHA256 != configDigest(cfg) {
		return nil, ErrCorrupt
	}
	db, err := openDB(cfg.Path)
	if err != nil {
		return nil, err
	}
	s = &Store{cfg: cfg, db: db, commit: db.Update}
	defer func() {
		if recover() != nil {
			err = ErrCorrupt
		}
		if err != nil {
			db.Close()
			s = nil
		}
	}()
	err = db.View(func(tx *bolt.Tx) error {
		if e := tx.ForEach(func(n []byte, _ *bolt.Bucket) error {
			if !bytes.Equal(n, bucket) {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		bk := tx.Bucket(bucket)
		if bk == nil || bk.Stats().KeyN != 1 {
			return ErrCorrupt
		}
		var checked checked
		if canonical(bk.Get(stateKey), &checked, cfg.MaxBytes*2+256) != nil || hash(checked.Data) != checked.SHA256 || canonical(checked.Data, &s.state, cfg.MaxBytes) != nil {
			return ErrCorrupt
		}
		if s.state.Version != Version || s.state.Marker != m || s.state.Physical.Validate() != nil || s.state.Sources == nil || len(s.state.Sources) > cfg.MaxSources {
			return ErrCorrupt
		}
		for k, r := range s.state.Sources {
			if e := s.restoreRow(k, r); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err = s.now(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) restoreRow(k string, r State) error {
	if r.Source.Validate() != nil || r.Source.Tenant != s.cfg.Tenant || k != key(r.Source) || r.Revision == 0 || r.LeaseRevision > r.Revision || r.FloorRevision > r.Revision || r.ThroughRevision > r.FloorRevision {
		return ErrCorrupt
	}
	if len(r.Lease) == 0 {
		if r.LeaseRevision != 0 || r.Epoch != 0 || r.BindingSHA256 != "" {
			return ErrCorrupt
		}
	} else {
		_, c, e := DecodeLease(r.Lease)
		if e != nil {
			return ErrCorrupt
		}
		w := s.state.Physical
		w.Earliest = time.UnixMilli(c.NotBeforeUnixMS)
		w.Latest = w.Earliest
		if _, _, _, e = s.leaseCrypto(r.Lease, w); e != nil || SourceOf(c.Scope) != r.Source || c.Epoch != r.Epoch || c.AuthorityRevision != r.LeaseRevision || binding(c) != r.BindingSHA256 {
			return ErrCorrupt
		}
	}
	if len(r.Floor) == 0 {
		if r.ThroughRevision != 0 || r.MinimumEpoch != 0 || r.FloorRevision != 0 {
			return ErrCorrupt
		}
	} else {
		_, c, e := DecodeFloor(r.Floor)
		if e != nil {
			return ErrCorrupt
		}
		w := s.state.Physical
		w.Earliest = time.UnixMilli(c.NotBeforeUnixMS)
		w.Latest = w.Earliest
		if _, _, _, e = s.floorCrypto(r.Floor, w); e != nil || c.Source != r.Source || c.ThroughRevision != r.ThroughRevision || c.MinimumEpoch != r.MinimumEpoch || c.AttestationRevision != r.FloorRevision {
			return ErrCorrupt
		}
	}
	return nil
}
func (s *Store) usable() error {
	if s.closed {
		return ErrClosed
	}
	if s.faulted {
		return ErrFaulted
	}
	return nil
}
func (s *Store) now(ctx context.Context) (trustedtime.Interval, error) {
	if e := ctx.Err(); e != nil {
		return trustedtime.Interval{}, e
	}
	w, e := s.cfg.Clock.Now()
	if e != nil || w.Validate() != nil {
		s.faulted = true
		return w, ErrClock
	}
	for _, p := range []trustedtime.Interval{s.state.Physical, s.observed} {
		if p.Earliest.IsZero() {
			continue
		}
		if w.Earliest.Before(p.Earliest) || w.Latest.Before(p.Latest) || p.BootID == w.BootID && w.Monotonic < p.Monotonic {
			s.faulted = true
			return w, ErrClock
		}
	}
	if !s.observed.Earliest.IsZero() && w.BootID != s.observed.BootID {
		s.faulted = true
		return w, ErrClock
	}
	s.observed = w
	return w, nil
}
func (s *Store) next() disk {
	d := s.state
	d.Sources = make(map[string]State, len(s.state.Sources))
	for k, v := range s.state.Sources {
		d.Sources[k] = v
	}
	return d
}
func (s *Store) persist(ctx context.Context, d disk, check func() error) error {
	if len(d.Sources) > s.cfg.MaxSources {
		return ErrCapacity
	}
	if _, e := storeBytes(d, s.cfg.MaxBytes); e != nil {
		return e
	}
	committing := false
	err := s.commit(func(tx *bolt.Tx) error {
		if check != nil {
			if e := check(); e != nil {
				return e
			}
			d.Physical = s.observed
		} else {
			w, e := s.now(ctx)
			if e != nil {
				return e
			}
			d.Physical = w
		}
		b, e := storeBytes(d, s.cfg.MaxBytes)
		if e != nil {
			return e
		}
		if e = tx.Bucket(bucket).Put(stateKey, b); e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		committing = true
		return nil
	})
	if err != nil {
		if committing {
			s.faulted = true
			return ErrFaulted
		}
		return err
	}
	s.state = d
	if check != nil {
		if e := check(); e != nil {
			s.faulted = true
			return ErrFaulted
		}
	}
	if check == nil {
		if _, e := s.now(ctx); e != nil {
			s.faulted = true
			return ErrFaulted
		}
	}
	return nil
}
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}
