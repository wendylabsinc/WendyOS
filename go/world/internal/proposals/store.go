package proposals

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"golang.org/x/sys/unix"
)

type scope struct {
	Tenant   string                         `json:"tenant"`
	Site     string                         `json:"site"`
	RunID    string                         `json:"run_id"`
	Audience string                         `json:"audience"`
	Assignee string                         `json:"assignee"`
	Catalog  map[string]mission.SafetyClass `json:"catalog"`
}
type journalEntry struct {
	Version  int     `json:"version"`
	Previous string  `json:"previous"`
	Scope    *scope  `json:"scope,omitempty"`
	Record   *Record `json:"record,omitempty"`
	SHA256   string  `json:"sha256"`
}
type Store struct {
	mu         sync.Mutex
	cfg        Config
	file, lock *os.File
	lastHash   string
	lastUnixMS int64
	bytes      int
	records    map[string]Record
	fault      error
	closed     bool
	// Test seam for uncertain persistence. Production always appends then fsyncs.
	append func([]byte) error
}

func boundedText(s string) bool { return len(s) > 0 && len(s) <= 256 }
func hash(b []byte) string      { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func copyRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Store) scope() scope {
	return scope{s.cfg.Tenant, s.cfg.Site, s.cfg.RunID, s.cfg.Audience, s.cfg.Assignee, s.cfg.Catalog}
}
func (s *Store) MatchesScope(tenant, site, runID, audience, assignee string) bool {
	return s != nil && s.cfg.Tenant == tenant && s.cfg.Site == site && s.cfg.RunID == runID && s.cfg.Audience == audience && s.cfg.Assignee == assignee
}
func (s *Store) CatalogMatches(catalog map[string]mission.SafetyClass) bool {
	return s != nil && reflect.DeepEqual(s.cfg.Catalog, catalog)
}

func Open(path string, cfg Config) (_ *Store, err error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = 1000
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 16 << 20
	}
	if path == "" || !boundedText(cfg.Tenant) || !boundedText(cfg.Site) || !ValidID(cfg.RunID) || !boundedText(cfg.Audience) || !boundedText(cfg.Assignee) || len(cfg.Catalog) == 0 || len(cfg.Catalog) > 100 || cfg.MaxEntries < 1 || cfg.MaxEntries > 10000 || cfg.MaxBytes < MaxRecordBytes || cfg.MaxBytes > 64<<20 {
		return nil, ErrInvalid
	}
	catalog := map[string]mission.SafetyClass{}
	for typ, class := range cfg.Catalog {
		if !boundedText(typ) || (class != mission.Observe && class != mission.Move && class != mission.Actuate) {
			return nil, ErrInvalid
		}
		catalog[typ] = class
	}
	cfg.Catalog = catalog
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	s := &Store{cfg: cfg, records: map[string]Record{}}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	open := func(p string, flags int) (*os.File, error) {
		fd, e := unix.Open(p, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
		if e != nil {
			return nil, e
		}
		f := os.NewFile(uintptr(fd), p)
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			f.Close()
			return nil, ErrUnavailable
		}
		return f, nil
	}
	s.lock, err = open(path+".lock", unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	s.file, err = open(path, unix.O_CREAT|unix.O_RDWR|unix.O_APPEND)
	if err != nil {
		return nil, err
	}
	fi, _ := s.file.Stat()
	li, _ := s.lock.Stat()
	if os.SameFile(fi, li) {
		return nil, ErrUnavailable
	}
	if fi.Size() > int64(cfg.MaxBytes) {
		return nil, ErrCapacity
	}
	s.append = func(data []byte) error {
		n, e := s.file.Write(data)
		if e == nil && n != len(data) {
			e = io.ErrShortWrite
		}
		if e == nil {
			e = s.file.Sync()
		}
		return e
	}
	reader := bufio.NewReaderSize(s.file, MaxRecordBytes+4096)
	for {
		line, e := reader.ReadBytes('\n')
		if len(line) > MaxRecordBytes+4096 {
			return nil, ErrUnavailable
		}
		if e == io.EOF && len(line) == 0 {
			break
		}
		if e != nil {
			return nil, ErrUnavailable
		} // No silent repair of partial writes.
		var entry journalEntry
		if authority.DecodeStrictLimit(line, &entry, MaxRecordBytes+4096) != nil || entry.Version != Version || entry.Previous != s.lastHash {
			return nil, ErrUnavailable
		}
		want := entry.SHA256
		entry.SHA256 = ""
		canonical, _ := json.Marshal(entry)
		if hash(canonical) != want {
			return nil, ErrUnavailable
		}
		if s.bytes == 0 {
			if entry.Scope == nil || entry.Record != nil || !reflect.DeepEqual(*entry.Scope, s.scope()) {
				return nil, ErrScope
			}
		} else {
			if entry.Scope != nil || entry.Record == nil || entry.Record.Sequence != uint64(len(s.records)+1) || len(s.records) >= cfg.MaxEntries {
				return nil, ErrUnavailable
			}
			r := *entry.Record
			if _, ok := s.records[r.Proposal.ID]; ok {
				return nil, ErrUnavailable
			}
			if err = s.validateStored(r); err != nil {
				return nil, err
			}
			if r.AcceptedAtUnixMS < s.lastUnixMS {
				return nil, ErrUnavailable
			}
			s.records[r.Proposal.ID] = r
			s.lastUnixMS = r.AcceptedAtUnixMS
		}
		s.bytes += len(line)
		s.lastHash = want
	}
	if cfg.Now().UnixMilli() < s.lastUnixMS {
		return nil, ErrUnavailable
	}
	if s.bytes == 0 {
		sc := s.scope()
		if err = s.commit(journalEntry{Scope: &sc}); err != nil {
			return nil, err
		}
		d, e := os.Open(dir)
		if e != nil {
			return nil, e
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) decode(body []byte) (Request, error) {
	var req Request
	if len(body) > MaxParametersBytes+4096 || authority.DecodeStrictLimit(body, &req, MaxParametersBytes+4096) != nil || req.Version != Version || !ValidID(req.ID) || len(req.Summary) > 512 || req.NotAfterUnixMS <= 0 || len(req.Parameters) == 0 || len(req.Parameters) > MaxParametersBytes {
		return req, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || len(fields) != 10 {
		return req, ErrInvalid
	}
	for _, key := range []string{"version", "id", "tenant", "site", "run_id", "assignee", "type", "parameters", "summary", "not_after_unix_ms"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return req, ErrInvalid
		}
	}
	var parameters map[string]json.RawMessage
	if authority.DecodeStrictLimit(req.Parameters, &parameters, MaxParametersBytes) != nil || parameters == nil {
		return req, ErrInvalid
	}
	if req.Tenant != s.cfg.Tenant || req.Site != s.cfg.Site || req.RunID != s.cfg.RunID || req.Assignee != s.cfg.Assignee {
		return req, ErrScope
	}
	if _, ok := s.cfg.Catalog[req.Type]; !ok {
		return req, ErrScope
	}
	return req, nil
}

// validateStored checks the journal's internal bindings. The checksum/hash chain
// detects corruption, not a hostile rewrite of the entire local filesystem.
// Current grant/revocation checks are exclusively the node admission callback.
func (s *Store) validateStored(r Record) error {
	req, err := s.decode(r.Body)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(req)
	b, _ := json.Marshal(r.Proposal)
	g := r.Proof.Grant.Grant
	if !bytes.Equal(a, b) || r.BodySHA256 != hash(r.Body) || r.Proof.BodySHA256 != r.BodySHA256 || r.Proof.Version != authority.Version || r.Proof.Operation != "propose" || r.Proof.Audience != s.cfg.Audience || g.Tenant != s.cfg.Tenant || g.Site != s.cfg.Site || g.Domain != "simulation" || (g.Kind != "agent" && g.Kind != "operator") || r.Principal != g.Principal || r.Kind != g.Kind || r.Source != g.Source || r.GrantID != g.ID || r.Class != s.cfg.Catalog[req.Type] || r.AcceptedAtUnixMS <= 0 || r.AcceptedAtUnixMS < r.Proof.IssuedAtUnixMS || r.AcceptedAtUnixMS >= r.Proof.ExpiresAtUnixMS || r.AcceptedAtUnixMS >= req.NotAfterUnixMS || req.NotAfterUnixMS > g.NotAfterUnixMS || req.NotAfterUnixMS-r.AcceptedAtUnixMS > MaxLifetime.Milliseconds() {
		return ErrInvalid
	}
	return nil
}

func (s *Store) ready() error {
	if s.closed || s.fault != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Store) commit(entry journalEntry) error {
	entry.Version = Version
	entry.Previous = s.lastHash
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	entry.SHA256 = hash(data)
	data, err = json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > MaxRecordBytes+4096 || s.bytes+len(data) > s.cfg.MaxBytes {
		return ErrCapacity
	}
	if err = s.append(data); err != nil {
		s.fault = err
		return ErrUnavailable
	}
	s.bytes += len(data)
	s.lastHash = entry.SHA256
	return nil
}

func (s *Store) current(ctx context.Context, req Request, proof authority.SignedRequest, validate func() error) (int64, error) {
	if ctx.Err() != nil || validate == nil {
		return 0, ErrAuthority
	}
	if err := validate(); err != nil {
		return 0, ErrAuthority
	}
	now := s.cfg.Now().UnixMilli()
	if ctx.Err() != nil || now < s.lastUnixMS || now < proof.IssuedAtUnixMS || now >= proof.ExpiresAtUnixMS || now >= proof.Grant.Grant.NotAfterUnixMS || now >= req.NotAfterUnixMS {
		return 0, ErrAuthority
	}
	return now, nil
}

// Submit requires a trusted current-authority check; it never verifies or
// creates operator instructions. Exact retries retain the first audit proof.
func (s *Store) Submit(ctx context.Context, body []byte, proof authority.SignedRequest, validate func() error) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Record{}, false, err
	}
	req, err := s.decode(body)
	if err != nil {
		return Record{}, false, err
	}
	now, err := s.current(ctx, req, proof, validate)
	if err != nil {
		return Record{}, false, err
	}
	g := proof.Grant.Grant
	r := Record{Sequence: uint64(len(s.records) + 1), Proposal: req, Principal: g.Principal, Kind: g.Kind, Source: g.Source, GrantID: g.ID, Class: s.cfg.Catalog[req.Type], AcceptedAtUnixMS: now, BodySHA256: hash(body), Body: bytes.Clone(body), Proof: proof}
	if err = s.validateStored(r); err != nil {
		return Record{}, false, err
	}
	if old, exists := s.records[req.ID]; exists {
		if old.Principal != r.Principal || old.Kind != r.Kind || old.Source != r.Source || !bytes.Equal(old.Body, body) {
			return Record{}, false, ErrConflict
		}
		return copyRecord(old), false, nil
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxRecordBytes {
		return Record{}, false, ErrInvalid
	}
	if len(s.records) >= s.cfg.MaxEntries {
		return Record{}, false, ErrCapacity
	}
	now, err = s.current(ctx, req, proof, validate)
	if err != nil {
		return Record{}, false, err
	}
	r.AcceptedAtUnixMS = now
	if err = s.commit(journalEntry{Record: &r}); err != nil {
		return Record{}, false, err
	}
	s.records[req.ID] = copyRecord(r)
	s.lastUnixMS = now
	// A durable historical proposal may remain if authority expires during I/O;
	// do not report current authorization or erase/reuse its ID on that failure.
	if _, err = s.current(ctx, req, proof, validate); err != nil {
		return Record{}, true, err
	}
	return copyRecord(r), true, nil
}

func (s *Store) Get(id string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return View{}, err
	}
	if !ValidID(id) {
		return View{}, ErrInvalid
	}
	r, ok := s.records[id]
	if !ok {
		return View{}, ErrNotFound
	}
	return View{Record: copyRecord(r), Expired: s.cfg.Now().UnixMilli() >= r.Proposal.NotAfterUnixMS}, nil
}
func (s *Store) List(limit int, after string) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Page{}, err
	}
	if limit < 1 || limit > 100 || (after != "" && !ValidID(after)) {
		return Page{}, ErrInvalid
	}
	keys := make([]string, 0, len(s.records))
	for id := range s.records {
		if id > after {
			keys = append(keys, id)
		}
	}
	sort.Strings(keys)
	out := Page{Proposals: []Summary{}}
	now := s.cfg.Now().UnixMilli()
	for _, id := range keys {
		r := s.records[id]
		q := r.Proposal
		item := Summary{q.ID, q.Type, q.Assignee, q.RunID, r.Principal, r.Kind, r.Source, r.Class, q.Summary, r.AcceptedAtUnixMS, q.NotAfterUnixMS, now >= q.NotAfterUnixMS}
		candidate := Page{Proposals: append(append([]Summary{}, out.Proposals...), item), Next: id}
		data, _ := json.Marshal(candidate)
		if len(out.Proposals) == limit || len(data) > MaxPageBytes-1 {
			out.Next = out.Proposals[len(out.Proposals)-1].ID
			return out, nil
		}
		out.Proposals = append(out.Proposals, item)
	}
	return out, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if s.file != nil {
		errs = append(errs, s.file.Close())
	}
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		errs = append(errs, s.lock.Close())
	}
	return errors.Join(errs...)
}

// Keep the path token contract independent of URL normalization.
func IDFromPath(path string) (string, bool) {
	id := strings.TrimPrefix(path, "/v1/proposals/")
	return id, ValidID(id) && path == "/v1/proposals/"+id
}
