package serviceevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"golang.org/x/sys/unix"
)

const stateName = ".service-evidence-state"
const lockName = ".service-evidence-lock"

func normalize(c Config) (Config, error) {
	if c.Clock == nil || !filepath.IsAbs(c.Directory) || filepath.Clean(c.Directory) != c.Directory || c.Directory == "/" || (Owner{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: "scope-validation", Service: "scope-validation", Component: "scope-validation", Stream: "scope-validation"}).Validate() != nil {
		return c, ErrDenied
	}
	if c.QuotaBytes == 0 {
		c.QuotaBytes = 128 << 20
	}
	if c.MaxBlobBytes == 0 {
		c.MaxBlobBytes = 8 << 20
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = 4096
	}
	if c.MaxRetention == 0 {
		c.MaxRetention = 7 * 24 * time.Hour
	}
	if c.MaxBlobBytes < 1 || c.MaxBlobBytes > 16<<20 || c.QuotaBytes < c.MaxBlobBytes || c.QuotaBytes > 1<<30 || c.MaxEntries < 1 || c.MaxEntries > 4096 || c.MaxRetention < time.Second || c.MaxRetention > 30*24*time.Hour {
		return c, ErrDenied
	}
	return c, nil
}
func configuration(c Config) string {
	b, _ := json.Marshal(struct {
		Tenant, Site, Device     string
		QuotaBytes, MaxBlobBytes int64
		MaxEntries               int
		MaxRetention             time.Duration
	}{c.Tenant, c.Site, c.Device, c.QuotaBytes, c.MaxBlobBytes, c.MaxEntries, c.MaxRetention})
	return digest(b)
}
func privateOpen(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "service-evidence-state")
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		f.Close()
		return nil, ErrCorrupt
	}
	return f, nil
}
func readState(path string) (diskState, error) {
	f, err := privateOpen(path, unix.O_RDONLY)
	if err != nil {
		return diskState{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	var checked checkedState
	var s diskState
	if err != nil || len(b) > 16<<10 || jws.DecodeStrict(b, &checked) != nil || checked.SHA256 != digest(checked.Data) || jws.DecodeStrict(checked.Data, &s) != nil || s.Version != 1 || s.Floor.Validate() != nil {
		return diskState{}, ErrCorrupt
	}
	canonical, _ := json.Marshal(s)
	if !bytes.Equal(canonical, checked.Data) {
		return diskState{}, ErrCorrupt
	}
	return s, nil
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) saveState(floor trustedtime.Interval, initial bool) error {
	next := s.state
	next.Floor = floor
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(checkedState{SHA256: digest(data), Data: data})
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.Directory, stateName)
	if initial {
		f, e := privateOpen(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
		if e != nil {
			return e
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		if ce := f.Close(); e == nil {
			e = ce
		}
		if e == nil {
			e = syncDirectory(s.cfg.Directory)
		}
		if e != nil {
			return e
		}
	} else {
		f, e := os.CreateTemp(s.cfg.Directory, ".state-")
		if e != nil {
			return e
		}
		name := f.Name()
		defer os.Remove(name)
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		if ce := f.Close(); e == nil {
			e = ce
		}
		if e != nil {
			return e
		}
		if e = os.Rename(name, path); e != nil {
			return e
		}
		if e = syncDirectory(s.cfg.Directory); e != nil {
			return e
		}
	}
	s.state = next
	return nil
}
func forward(a, b trustedtime.Interval) bool {
	return b.Validate() == nil && (a.Earliest.IsZero() || !b.Earliest.Before(a.Earliest) && !b.Latest.Before(a.Latest) && (a.BootID != b.BootID || b.Monotonic >= a.Monotonic))
}
func (s *Store) now() (trustedtime.Interval, error) {
	w, err := s.cfg.Clock.Now()
	if err != nil || !forward(s.state.Floor, w) || !forward(s.observed, w) || !s.observed.Earliest.IsZero() && w.BootID != s.observed.BootID {
		return trustedtime.Interval{}, ErrClock
	}
	s.observed = w
	return w, nil
}

// Open creates an empty source-owned store only in an empty protected directory.
// Existing blob history requires the matching scope/clock metadata. Corruption
// is never interpreted as a new store. Whole-volume rollback/deletion and a
// hostile same-UID/root writer require an external anchor and are out of scope.
func Open(cfg Config) (result *Store, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if err = hostpath.EnsureDirectory(cfg.Directory, 0700, true); err != nil {
		return nil, err
	}
	lock, err := privateOpen(filepath.Join(cfg.Directory, lockName), unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{cfg: cfg, lock: lock}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	state, readErr := readState(filepath.Join(cfg.Directory, stateName))
	initial := errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !initial {
		return nil, readErr
	}
	entries, err := os.ReadDir(cfg.Directory)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if initial {
			if e.Name() != lockName {
				return nil, ErrCorrupt
			}
		} else if e.Name() != lockName && e.Name() != stateName && e.Name() != "blobs" && !strings.HasPrefix(e.Name(), ".state-") {
			return nil, ErrCorrupt
		}
	}
	if initial {
		s.state = diskState{Version: 1, ConfigurationSHA256: configuration(cfg)}
	} else {
		if state.ConfigurationSHA256 != configuration(cfg) {
			return nil, ErrDenied
		}
		s.state = state
	}
	w, err := s.now()
	if err != nil {
		return nil, err
	}
	if err = s.saveState(w, initial); err != nil {
		return nil, err
	}
	s.storeNow = w.Latest
	blobs := filepath.Join(cfg.Directory, "blobs")
	if err = hostpath.EnsureDirectory(blobs, 0700, initial); err != nil {
		return nil, err
	}
	if err = checkBlobFiles(blobs); err != nil {
		return nil, err
	}
	s.inner, err = evidence.Open(blobs, evidence.Config{Tenant: cfg.Tenant, Site: cfg.Site, QuotaBytes: cfg.QuotaBytes, MaxBlobBytes: cfg.MaxBlobBytes, MaxEntries: cfg.MaxEntries, MaxRetention: cfg.MaxRetention, Now: func() time.Time { return s.storeNow }})
	if err != nil {
		return nil, err
	}
	after, err := s.now()
	if err != nil {
		return nil, errors.Join(ErrUncertain, err)
	}
	if err = s.saveState(after, false); err != nil {
		return nil, errors.Join(ErrUncertain, err)
	}
	if _, err = s.now(); err != nil {
		return nil, errors.Join(ErrUncertain, err)
	}
	// Ignore only uncommitted private metadata temporaries after validating the
	// committed scope and physical floor. They never replace committed state.
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			if err = os.Remove(filepath.Join(cfg.Directory, e.Name())); err != nil {
				return nil, err
			}
		}
	}
	ok = true
	return s, nil
}
func checkBlobFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		f, e := privateOpen(filepath.Join(dir, entry.Name()), unix.O_RDONLY)
		if e != nil {
			return ErrCorrupt
		}
		f.Close()
	}
	return nil
}
