// Package evidence stores digest-addressed capture bytes independently of world state.
package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const MaxChunk = 48 << 10
const DefaultMaxBlob = 8 << 20

var ErrUnavailable = errors.New("evidence unavailable or expired")
var ErrQuota = errors.New("evidence quota exhausted")

type Config struct {
	ModalityBudgets          map[string]ModalityBudget
	Tenant, Site             string
	QuotaBytes, MaxBlobBytes int64
	MaxRetention             time.Duration
	MaxEntries               int
	MaxPins                  int
	MaxPinsPerOwner          int
	MaxPinReferences         int
	RollingUploadTimeout     time.Duration
	Now                      func() time.Time
}
type PutRequest struct {
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	Offset            int64  `json:"offset"`
	Data              []byte `json:"data"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
	RetentionClass    string `json:"retention_class,omitempty"`
}
type PutResponse struct {
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	Received          int64  `json:"received"`
	Complete          bool   `json:"complete"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
}
type GetRequest struct {
	SHA256 string `json:"sha256"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}
type GetResponse struct {
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	Offset            int64  `json:"offset"`
	Data              []byte `json:"data"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
}
type manifest struct {
	Version           int    `json:"version"`
	Tenant            string `json:"tenant"`
	Site              string `json:"site"`
	Owner             string `json:"owner"`
	RetentionClass    string `json:"retention_class,omitempty"`
	Allocation        uint64 `json:"allocation,omitempty"`
	UploadUntilUnixMS int64  `json:"upload_until_unix_ms,omitempty"`
	PutResponse
}
type Store struct {
	mu             sync.Mutex
	dir            string
	cfg            Config
	entries        map[string]manifest
	lock           *os.File
	closed         bool
	fault          error
	pins           map[string]pinRecord
	pinned         map[string]int
	nextAllocation uint64
	pinCommitHook  func(string) error
	removeHook     func(string) error
}

func Open(dir string, cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.QuotaBytes == 0 {
		cfg.QuotaBytes = 128 << 20
	}
	if cfg.MaxBlobBytes == 0 {
		cfg.MaxBlobBytes = DefaultMaxBlob
	}
	if cfg.MaxRetention == 0 {
		cfg.MaxRetention = 7 * 24 * time.Hour
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = 4096
	}
	if err := retentionDefaults(&cfg); err != nil {
		return nil, err
	}
	if cfg.Tenant == "" || cfg.Site == "" || dir == "" || cfg.MaxBlobBytes < 1 || cfg.MaxBlobBytes > 64<<20 || cfg.QuotaBytes < cfg.MaxBlobBytes || cfg.QuotaBytes > 1<<40 || cfg.MaxRetention < time.Second || cfg.MaxRetention > 30*24*time.Hour || cfg.MaxEntries < 1 || cfg.MaxEntries > 100000 {
		return nil, errors.New("invalid evidence configuration")
	}
	budgets, err := NormalizeModalityBudgets(cfg.ModalityBudgets, cfg.QuotaBytes, cfg.MaxRetention)
	if err != nil {
		return nil, err
	}
	cfg.ModalityBudgets = budgets
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".lock")
	if st, e := os.Lstat(lockPath); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("evidence lock cannot be symlink")
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{dir: dir, cfg: cfg, entries: map[string]manifest{}, lock: lock, pins: map[string]pinRecord{}, pinned: map[string]int{}, nextAllocation: 1}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if err = s.loadPins(); err != nil {
		return nil, err
	}
	if err = s.prunePins(); err != nil {
		return nil, err
	}
	manifests := 0
	hasV2 := false
	for _, f := range files {
		if f.Name() == retentionFile {
			continue
		}
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		manifests++
		if manifests > 100000 {
			return nil, ErrQuota
		}
		digest := strings.TrimSuffix(f.Name(), ".json")
		if !validDigest(digest) || !f.Type().IsRegular() {
			return nil, errors.New("invalid evidence manifest path")
		}
		b, e := readRegular(filepath.Join(dir, f.Name()), 8192)
		if e != nil {
			return nil, e
		}
		var m manifest
		if e = decodeDisk(b, &m); e != nil {
			return nil, e
		}
		if (m.Version != 1 && m.Version != 2) || m.Tenant != cfg.Tenant || m.Site != cfg.Site || m.Owner == "" || m.SHA256 != digest || m.Size < 1 || m.Size > cfg.MaxBlobBytes || m.Received < 0 || m.Received > m.Size || m.Complete != (m.Received == m.Size) || !validType(m.ContentType) || m.RetainUntilUnixMS <= 0 || !validManifestRetention(m) {
			return nil, errors.New("invalid evidence manifest")
		}
		if m.RetentionClass == "" {
			m.RetentionClass = RetentionProtected
		}
		if m.Version == 2 {
			hasV2 = true
		}
		if expiredManifest(m, cfg.Now().UnixMilli()) {
			if e = s.remove(digest); e != nil {
				return nil, e
			}
			continue
		}
		dataPath := s.blob(digest)
		st, e := os.Lstat(dataPath)
		if e != nil || !st.Mode().IsRegular() || st.Size() < m.Received {
			return nil, errors.New("missing or truncated evidence bytes")
		}
		if st.Size() > m.Received {
			if e = os.Truncate(dataPath, m.Received); e != nil {
				return nil, e
			}
		}
		if m.Complete {
			if e = s.verify(m); e != nil {
				return nil, e
			}
		}
		s.entries[digest] = m
		if s.nextAllocation != 0 && m.Allocation >= s.nextAllocation {
			s.nextAllocation = m.Allocation + 1
		}
	}
	if hasV2 {
		if _, err := os.Lstat(filepath.Join(s.dir, retentionFile)); err != nil {
			return nil, errors.New("evidence retention ledger missing")
		}
	}
	if err = s.validatePins(); err != nil {
		return nil, err
	}
	if err = s.makeRoom(0, 0, ""); err != nil {
		return nil, err
	}
	// An uncommitted initial blob or atomic-write temporary never becomes visible.
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".blob") {
			digest := strings.TrimSuffix(f.Name(), ".blob")
			if validDigest(digest) {
				if _, found := s.entries[digest]; !found {
					if err = os.Remove(filepath.Join(dir, f.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
						return nil, err
					}
				}
			}
		} else if strings.HasPrefix(f.Name(), ".manifest-") || strings.HasPrefix(f.Name(), ".retention-") {
			_ = os.Remove(filepath.Join(dir, f.Name()))
		}
	}
	ok = true
	return s, nil
}
func validDigest(d string) bool {
	b, e := hex.DecodeString(d)
	return e == nil && len(b) == sha256.Size && d == strings.ToLower(d)
}
func validType(t string) bool {
	return t == "image/png" || t == "image/jpeg" || t == "audio/wav" || t == "video/mp4" || t == "application/octet-stream" || t == "application/json"
}
func (s *Store) blob(d string) string { return filepath.Join(s.dir, d+".blob") }
func (s *Store) ready() error {
	if s.closed {
		return errors.New("evidence store closed")
	}
	return s.fault
}
func (s *Store) remove(d string) error {
	// Manifest removal first prevents an absent body from being advertised.
	for i, p := range []string{filepath.Join(s.dir, d+".json"), s.blob(d)} {
		if e := os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e := syncDir(s.dir); e != nil {
			return e
		}
		if i == 0 && s.removeHook != nil {
			if e := s.removeHook(d); e != nil {
				return e
			}
		}
	}
	delete(s.entries, d)
	return nil
}
func (s *Store) prune() error {
	if err := s.prunePins(); err != nil {
		return err
	}
	now := s.cfg.Now().UnixMilli()
	keys := []string{}
	for k, m := range s.entries {
		if expiredManifest(m, now) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if e := s.remove(k); e != nil {
			s.fault = e
			return e
		}
	}
	return nil
}
func (s *Store) save(m manifest) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.dir, ".manifest-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(f.Name(), filepath.Join(s.dir, m.SHA256+".json")); e != nil {
		return e
	}
	return syncDir(s.dir)
}
func syncDir(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func readRegular(path string, limit int64) ([]byte, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("invalid evidence file")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e == nil && int64(len(b)) > limit {
		e = errors.New("evidence file too large")
	}
	return b, e
}
func (s *Store) verify(m manifest) error { _, e := s.verified(m); return e }
func (s *Store) verified(m manifest) ([]byte, error) {
	b, e := readRegular(s.blob(m.SHA256), m.Size)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != m.Size || hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, errors.New("evidence digest mismatch")
	}
	return b, nil
}

// Put commits bytes before their manifest and acknowledgement. Empty chunks may
// query an upload's committed offset. A different owner cannot alter its metadata.
func (s *Store) Put(owner string, r PutRequest) (PutResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return PutResponse{}, e
	}
	if e := s.prune(); e != nil {
		return PutResponse{}, e
	}
	if owner == "" || len(owner) > 512 || !validDigest(r.SHA256) || r.Size < 1 || r.Size > s.cfg.MaxBlobBytes || r.Offset < 0 || r.Offset > r.Size || len(r.Data) > MaxChunk || int64(len(r.Data)) > r.Size-r.Offset || !validType(r.ContentType) {
		return PutResponse{}, errors.New("invalid evidence chunk")
	}
	class := r.RetentionClass
	if class == "" {
		class = RetentionProtected
	}
	if class != RetentionProtected && class != RetentionRolling {
		return PutResponse{}, errors.New("invalid evidence retention class")
	}
	m, found := s.entries[r.SHA256]
	now := s.cfg.Now()
	if found {
		stronger := class == RetentionRolling && m.RetentionClass == RetentionProtected && m.Complete && r.RetainUntilUnixMS == 0
		if m.Owner != owner || m.Size != r.Size || m.ContentType != r.ContentType || (m.RetentionClass != class && !stronger) || (r.RetainUntilUnixMS != 0 && m.RetainUntilUnixMS != r.RetainUntilUnixMS) {
			return PutResponse{}, errors.New("evidence owner or metadata differs")
		}
		// A durable completion manifest is not proof that current disk bytes still
		// exist or match. Every completion acknowledgement verifies them again.
		if m.Complete {
			if e := s.verify(m); e != nil {
				return PutResponse{}, fmt.Errorf("evidence integrity: %w", e)
			}
		}
	} else {
		if r.Offset != 0 {
			return PutResponse{}, errors.New("new upload must start at zero")
		}
		retention := time.Duration(s.cfg.ModalityBudgets[modality(r.ContentType)].MaxRetentionMS) * time.Millisecond
		until := r.RetainUntilUnixMS
		if until == 0 {
			until = now.Add(retention).UnixMilli()
		}
		if until <= now.UnixMilli() || until > now.Add(retention).UnixMilli() {
			return PutResponse{}, errors.New("invalid evidence retention")
		}
		if s.nextAllocation == 0 {
			return PutResponse{}, errors.New("evidence allocation order exhausted")
		}
		if e := s.ensureRetentionMarker(); e != nil {
			return PutResponse{}, e
		}
		if e := s.makeRoom(r.Size, 1, modality(r.ContentType)); e != nil {
			return PutResponse{}, e
		}
		f, e := os.OpenFile(s.blob(r.SHA256), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return PutResponse{}, e
		}
		e = f.Sync()
		ce := f.Close()
		if e != nil {
			return PutResponse{}, e
		}
		if ce != nil {
			return PutResponse{}, ce
		}
		m = manifest{Version: 2, Tenant: s.cfg.Tenant, Site: s.cfg.Site, Owner: owner, RetentionClass: class, Allocation: s.nextAllocation, PutResponse: PutResponse{SHA256: r.SHA256, Size: r.Size, ContentType: r.ContentType, RetainUntilUnixMS: until}}
		if class == RetentionRolling {
			m.UploadUntilUnixMS = min(until, now.Add(s.cfg.RollingUploadTimeout).UnixMilli())
		}
		if e = s.save(m); e != nil {
			s.fault = e
			return PutResponse{}, e
		}
		s.entries[r.SHA256] = m
		s.nextAllocation++
	}
	if r.Offset > m.Received {
		return PutResponse{}, errors.New("chunk leaves an upload gap")
	}
	if len(r.Data) == 0 {
		return m.PutResponse, nil
	}
	if r.Offset < m.Received {
		if r.Offset+int64(len(r.Data)) > m.Received {
			return PutResponse{}, errors.New("chunk partially overlaps committed bytes")
		}
		f, e := os.Open(s.blob(r.SHA256))
		if e != nil {
			return PutResponse{}, e
		}
		b := make([]byte, len(r.Data))
		_, e = f.ReadAt(b, r.Offset)
		f.Close()
		if e != nil {
			return PutResponse{}, e
		}
		if !bytes.Equal(b, r.Data) {
			return PutResponse{}, errors.New("retried chunk differs")
		}
		return m.PutResponse, nil
	}
	if m.Version == 1 {
		if s.nextAllocation == 0 {
			return PutResponse{}, errors.New("evidence allocation order exhausted")
		}
		if e := s.ensureRetentionMarker(); e != nil {
			return PutResponse{}, e
		}
		m.Version, m.Allocation = 2, s.nextAllocation
		s.nextAllocation++
	}
	f, e := os.OpenFile(s.blob(r.SHA256), os.O_WRONLY, 0600)
	if e != nil {
		return PutResponse{}, e
	}
	_, e = f.WriteAt(r.Data, r.Offset)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		s.fault = e
		return PutResponse{}, e
	}
	m.Received += int64(len(r.Data))
	m.Complete = m.Received == m.Size
	if m.Complete {
		if e = s.verify(m); e != nil {
			if removeErr := s.remove(m.SHA256); removeErr != nil {
				s.fault = removeErr
			}
			return PutResponse{}, e
		}
	}
	if e = s.save(m); e != nil {
		s.fault = e
		return PutResponse{}, e
	}
	s.entries[m.SHA256] = m
	return m.PutResponse, nil
}
func (s *Store) Get(r GetRequest) (GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return GetResponse{}, e
	}
	if !validDigest(r.SHA256) || r.Offset < 0 || r.Length < 1 || r.Length > MaxChunk {
		return GetResponse{}, errors.New("invalid evidence range")
	}
	if e := s.prune(); e != nil {
		return GetResponse{}, e
	}
	m, ok := s.entries[r.SHA256]
	if !ok || !m.Complete {
		return GetResponse{}, ErrUnavailable
	}
	if r.Offset >= m.Size {
		return GetResponse{}, errors.New("evidence offset outside blob")
	}
	verified, e := s.verified(m)
	if e != nil {
		return GetResponse{}, fmt.Errorf("evidence integrity: %w", e)
	}
	length := int64(r.Length)
	if length > m.Size-r.Offset {
		length = m.Size - r.Offset
	}
	data := append([]byte(nil), verified[r.Offset:r.Offset+length]...)
	return GetResponse{SHA256: m.SHA256, Size: m.Size, Offset: r.Offset, Data: data, ContentType: m.ContentType, RetainUntilUnixMS: m.RetainUntilUnixMS}, nil
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

// MatchesScope prevents accidentally mounting one site's media store into another.
func (s *Store) MatchesScope(tenant, site string) bool {
	return s.cfg.Tenant == tenant && s.cfg.Site == site
}
