package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

type checkpoint struct {
	Earliest  time.Time     `json:"earliest"`
	BootID    string        `json:"boot_id"`
	Monotonic time.Duration `json:"monotonic"`
}
type state struct {
	Version     int              `json:"version"`
	ScopeDigest string           `json:"scope_digest"`
	Policy      retained         `json:"policy"`
	Revocations retained         `json:"revocations"`
	Nonces      map[string]int64 `json:"nonces"`
	Time        checkpoint       `json:"time"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}

type Store struct {
	mu              sync.Mutex
	cfg             Config
	path            string
	lock            *os.File
	state           state
	closed, faulted bool
	write           func(string, []byte) error
}

func normalize(c Config) (Config, error) {
	if c.MaxPolicyLifetime == 0 {
		c.MaxPolicyLifetime = 24 * time.Hour
	}
	if c.MaxRequestLifetime == 0 {
		c.MaxRequestLifetime = time.Minute
	}
	if c.MaxNonces == 0 {
		c.MaxNonces = 10000
	}
	if c.MaxPolicyEntries == 0 {
		c.MaxPolicyEntries = 1024
	}
	if c.MaxRevoked == 0 {
		c.MaxRevoked = 4096
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 8 << 20
	}
	if !tokenPattern.MatchString(c.Tenant) || !tokenPattern.MatchString(c.Site) || c.Audience == "" || len(c.Audience) > 512 || bytes.IndexAny([]byte(c.Audience), "\x00\r\n") >= 0 || c.Certificates == nil || c.MaxRevocationAge < time.Millisecond || c.MaxRevocationAge > 24*time.Hour || c.MaxPolicyLifetime < time.Millisecond || c.MaxPolicyLifetime > 30*24*time.Hour || c.MaxRequestLifetime < time.Millisecond || c.MaxRequestLifetime > time.Minute || c.MaxNonces < 1 || c.MaxNonces > 100000 || c.MaxPolicyEntries < 1 || c.MaxPolicyEntries > 4096 || c.MaxRevoked < 1 || c.MaxRevoked > 16384 || c.MaxBytes < 64<<10 || c.MaxBytes > 32<<20 {
		return c, ErrDenied
	}
	for _, pin := range []Pin{c.PolicyIssuer, c.RevocationIssuer} {
		if !canonicalPrincipal(pin.Principal, c.Tenant) || !digestPattern.MatchString(pin.SigningKeySHA256) {
			return c, ErrDenied
		}
	}
	return c, nil
}
func scopeDigest(c Config) string {
	b, _ := json.Marshal(struct {
		Tenant, Site, Audience                                  string
		PolicyIssuer, RevocationIssuer                          Pin
		MaxRevocationAge, MaxPolicyLifetime, MaxRequestLifetime time.Duration
		MaxNonces, MaxPolicyEntries, MaxRevoked, MaxBytes       int
	}{c.Tenant, c.Site, c.Audience, c.PolicyIssuer, c.RevocationIssuer, c.MaxRevocationAge, c.MaxPolicyLifetime, c.MaxRequestLifetime, c.MaxNonces, c.MaxPolicyEntries, c.MaxRevoked, c.MaxBytes})
	return digest(b)
}

// Open needs explicit commissioned identity pins. Persistence assumes trusted
// local storage; its checksum detects damage, not malicious whole-volume rollback.
func Open(path string, cfg Config) (s *Store, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, ErrDenied
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if info, e := lock.Stat(); e != nil || !info.Mode().IsRegular() {
		_ = lock.Close()
		return nil, errors.New("ledger lock must be a regular file")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("ledger already open: %w", err)
	}
	s = &Store{cfg: cfg, path: path, lock: lock, write: atomicWrite, state: state{Version: 1, ScopeDigest: scopeDigest(cfg), Nonces: map[string]int64{}}}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	f, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(openErr, os.ErrNotExist) {
		err = s.commit(s.state)
		return s, err
	}
	if openErr != nil {
		return s, openErr
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return s, errors.New("ledger must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(cfg.MaxBytes)+1))
	_ = f.Close()
	if err != nil {
		return s, err
	}
	if len(b) > cfg.MaxBytes {
		return s, ErrCapacity
	}
	var disk envelope
	if err = decodeDisk(b, &disk); err != nil {
		return s, err
	}
	if digest(disk.State) != disk.SHA256 {
		return s, errors.New("ledger checksum mismatch")
	}
	if err = decodeDisk(disk.State, &s.state); err != nil {
		return s, err
	}
	if err = s.validateState(); err != nil {
		return s, err
	}
	return s, nil
}

func (s *Store) validateState() error {
	st := s.state
	if st.Version != 1 || st.ScopeDigest != scopeDigest(s.cfg) || st.Nonces == nil || len(st.Nonces) > s.cfg.MaxNonces {
		return ErrDenied
	}
	for key, expiry := range st.Nonces {
		if !digestPattern.MatchString(key) || expiry <= 0 {
			return ErrDenied
		}
	}
	if st.Time.Earliest.IsZero() {
		if st.Time.BootID != "" || st.Time.Monotonic != 0 || st.Policy.Revision != 0 || st.Revocations.Revision != 0 || len(st.Nonces) != 0 {
			return ErrDenied
		}
	} else if st.Time.Earliest.UnixMilli() <= 0 || st.Time.BootID == "" || len(st.Time.BootID) > 256 || st.Time.Monotonic < 0 {
		return ErrDenied
	}
	for _, item := range []struct {
		r      retained
		policy bool
	}{{st.Policy, true}, {st.Revocations, false}} {
		if item.r.Revision == 0 {
			if item.r.Proof != "" || item.r.Digest != "" || item.r.VerifiedAt != (trustedtime.Interval{}) {
				return ErrDenied
			}
			continue
		}
		if item.r.VerifiedAt.Validate() != nil || item.r.VerifiedAt.Earliest.After(st.Time.Earliest) {
			return ErrDenied
		}
		if item.policy {
			p, _, hash, err := s.verifyPolicy(item.r.Proof, item.r.VerifiedAt)
			if err != nil {
				return err
			}
			if p.Revision != item.r.Revision || hash != item.r.Digest {
				return ErrDenied
			}
		} else {
			r, _, hash, err := s.verifyRevocations(item.r.Proof, item.r.VerifiedAt)
			if err != nil {
				return err
			}
			if r.Revision != item.r.Revision || hash != item.r.Digest {
				return ErrDenied
			}
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
func (s *Store) window(w trustedtime.Interval) (trustedtime.Interval, error) {
	if err := w.Validate(); err != nil {
		return w, err
	}
	c := s.state.Time
	if !c.Earliest.IsZero() {
		if w.Latest.Before(c.Earliest) || (w.BootID == c.BootID && w.Monotonic < c.Monotonic) {
			return w, ErrRollback
		}
		if w.Earliest.Before(c.Earliest) {
			w.Earliest = c.Earliest
		}
	}
	return w, nil
}
func withTime(st state, w trustedtime.Interval) state {
	st.Time = checkpoint{w.Earliest, w.BootID, w.Monotonic}
	return st
}
func revision(old retained, version uint64, hash string) error {
	if version < old.Revision {
		return ErrRollback
	}
	if version == old.Revision && hash != old.Digest {
		return ErrConflict
	}
	return nil
}

func (s *Store) ApplyPolicy(proof string, w trustedtime.Interval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	w, err := s.window(w)
	if err != nil {
		return err
	}
	p, id, hash, err := s.verifyPolicy(proof, w)
	if err != nil {
		return err
	}
	if err = revision(s.state.Policy, p.Revision, hash); err != nil {
		return err
	}
	if s.state.Revocations.Revision != 0 {
		r, revIssuer, _, e := s.verifyRevocations(s.state.Revocations.Proof, w)
		if e != nil {
			return e
		}
		if revoked(r, id) || revoked(r, revIssuer) || contains(r.RevokedPolicyIDs, p.PolicyID) {
			return ErrDenied
		}
	}
	next := withTime(s.state, w)
	next.Policy = retained{p.Revision, hash, proof, w}
	return s.commit(next)
}
func (s *Store) ApplyRevocations(proof string, w trustedtime.Interval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	w, err := s.window(w)
	if err != nil {
		return err
	}
	r, id, hash, err := s.verifyRevocations(proof, w)
	if err != nil {
		return err
	}
	if err = revision(s.state.Revocations, r.Revision, hash); err != nil {
		return err
	}
	// Expired deny state cannot be used as authority, but its previously
	// verified revocations still prevent a revoked issuer from clearing itself.
	if s.state.Revocations.Revision != 0 {
		old, _, _, e := s.verifyRevocations(s.state.Revocations.Proof, s.state.Revocations.VerifiedAt)
		if e != nil {
			return e
		}
		if revoked(old, id) {
			return ErrDenied
		}
	}
	next := withTime(s.state, w)
	next.Revocations = retained{r.Revision, hash, proof, w}
	return s.commit(next)
}

// Check is nonconsuming revalidation, never admission. Only Admit durably
// consumes replay state. A failed post-commit Check must leave its nonce spent.
func (s *Store) Check(req Request, w trustedtime.Interval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	w, err := s.window(w)
	if err != nil {
		return err
	}
	return s.checkAuthority(req, w)
}
func (s *Store) Admit(req Request, w trustedtime.Interval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	w, err := s.window(w)
	if err != nil {
		return err
	}
	if err = s.checkAuthority(req, w); err != nil {
		return err
	}
	key := digest([]byte(req.Identity.SigningKeySHA256 + "\x00" + s.cfg.Audience + "\x00" + req.Nonce))
	if expiry, exists := s.state.Nonces[key]; exists && expiry > w.Earliest.UnixMilli() {
		return ErrReplay
	}
	next := withTime(s.state, w)
	next.Nonces = map[string]int64{}
	for nonce, expiry := range s.state.Nonces {
		if expiry > w.Earliest.UnixMilli() {
			next.Nonces[nonce] = expiry
		}
	}
	if len(next.Nonces) >= s.cfg.MaxNonces {
		return ErrCapacity
	}
	next.Nonces[key] = req.ExpiresAtUnixMS
	return s.commit(next)
}
func (s *Store) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Snapshot{}, err
	}
	out := Snapshot{PolicyRevision: s.state.Policy.Revision, RevocationRevision: s.state.Revocations.Revision, PolicyDigest: s.state.Policy.Digest, RevocationDigest: s.state.Revocations.Digest, NonceCount: len(s.state.Nonces), EarliestCheckpoint: s.state.Time.Earliest}
	if s.state.Policy.Revision != 0 {
		p, _, _, err := s.verifyPolicy(s.state.Policy.Proof, s.state.Policy.VerifiedAt)
		if err != nil {
			return Snapshot{}, err
		}
		out.PolicyID = p.PolicyID
	}
	return out, nil
}
func (s *Store) commit(next state) error {
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	b, err := json.Marshal(envelope{digest(payload), payload})
	if err != nil {
		return err
	}
	if len(b) > s.cfg.MaxBytes {
		return ErrCapacity
	}
	if err = s.write(s.path, b); err != nil {
		s.faulted = true
		return fmt.Errorf("persist admission: %w", err)
	}
	s.state = next
	return nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		return s.lock.Close()
	}
	return nil
}
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".admission-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func decodeDisk(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return ErrDenied
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrDenied
				}
				seen[name] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrDenied
			}
		case json.Delim('['):
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrDenied
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrDenied
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
