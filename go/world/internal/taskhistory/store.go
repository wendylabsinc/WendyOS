// Package taskhistory retains assignee audit history separately from execution.
package taskhistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const MaxPageBytes = 1 << 20
const MaxStoreBytes = 32 << 20

var ErrInvalid = errors.New("invalid mission history or cursor")
var ErrConflict = errors.New("immutable mission history conflict")
var ErrCapacity = errors.New("mission history capacity exhausted")
var ErrUnavailable = errors.New("mission history unavailable")

type Origin struct {
	Principal string `json:"principal"`
	Assignee  string `json:"assignee"`
	RunID     string `json:"run_id"`
}
type Config struct {
	Tenant, Site         string
	Origins              map[string]Origin
	Verifier             *authority.Verifier
	MaxEntries, MaxBytes int
	Now                  func() time.Time
}
type Receipt = authority.SignedTaskAudit
type Request struct {
	Origin         string `json:"origin"`
	RunID          string `json:"run_id"`
	Incarnation    string `json:"incarnation"`
	After          uint64 `json:"after"`
	PreviousSHA256 string `json:"previous_sha256"`
}
type Page struct {
	Origin         string    `json:"origin"`
	RunID          string    `json:"run_id"`
	Incarnation    string    `json:"incarnation"`
	After          uint64    `json:"after"`
	Through        uint64    `json:"through"`
	PreviousSHA256 string    `json:"previous_sha256"`
	More           bool      `json:"more"`
	Records        []Receipt `json:"records"`
}
type Progress struct {
	Through          uint64 `json:"through"`
	Incarnation      string `json:"incarnation"`
	SHA256           string `json:"sha256"`
	LastSyncUnixMS   int64  `json:"last_sync_unix_ms"`
	Via              string `json:"via"`
	CaughtUpWithPeer bool   `json:"caught_up_with_peer"`
}
type View struct {
	Receipt                         Receipt       `json:"receipt"`
	Event                           tasking.Audit `json:"event"`
	HistoricalProvenanceVerified    bool          `json:"historical_provenance_verified"`
	CurrentVerification             string        `json:"current_verification"`
	OriginalInstructionVerification string        `json:"original_instruction_verification"`
	Executable                      bool          `json:"executable"`
}
type List struct {
	Origin     string   `json:"origin"`
	RunID      string   `json:"run_id"`
	TaskID     string   `json:"task_id"`
	Events     []View   `json:"events"`
	Next       string   `json:"next"`
	Progress   Progress `json:"progress"`
	Historical bool     `json:"historical"`
}
type state struct {
	Version    int                  `json:"version"`
	Tenant     string               `json:"tenant"`
	Site       string               `json:"site"`
	Origins    map[string]Origin    `json:"origins"`
	Records    map[string][]Receipt `json:"records"`
	Progress   map[string]Progress  `json:"progress"`
	LastUnixMS int64                `json:"last_unix_ms"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}
type Store struct {
	mu     sync.Mutex
	cfg    Config
	path   string
	lock   *os.File
	state  state
	fault  error
	closed bool
	write  func(string, []byte) error
}

func clone[T any](v T) T            { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func same(a, b any) bool            { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
func hash(b []byte) string          { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func name(v string) bool            { return len(v) > 0 && len(v) <= 256 && !strings.ContainsAny(v, "\x00\r\n") }
func zero() string                  { return strings.Repeat("0", 64) }
func event(r Receipt) tasking.Audit { var e tasking.Audit; _ = json.Unmarshal(r.Event, &e); return e }
func prefix(rs []Receipt) string {
	if len(rs) == 0 {
		return zero()
	}
	r := rs[len(rs)-1]
	return tasking.AuditEventHash(r.PreviousSHA256, event(r))
}
func ValidateConfig(c Config) error {
	if !name(c.Tenant) || !name(c.Site) || len(c.Origins) < 1 || len(c.Origins) > 8 || c.MaxEntries < 1 || c.MaxEntries > 10000 || c.MaxBytes < MaxPageBytes || c.MaxBytes > MaxStoreBytes {
		return ErrInvalid
	}
	for a, o := range c.Origins {
		if !name(a) || !name(o.Principal) || !name(o.Assignee) || !name(o.RunID) {
			return ErrInvalid
		}
	}
	return nil
}
func Open(path string, cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = 1000
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 16 << 20
	}
	if path == "" || cfg.Verifier == nil || ValidateConfig(cfg) != nil {
		return nil, ErrInvalid
	}
	cfg.Origins = clone(cfg.Origins)
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(abs))
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	st, err := lock.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, ErrUnavailable
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{cfg: cfg, path: path, lock: lock, write: writeAtomic, state: state{Version: 1, Tenant: cfg.Tenant, Site: cfg.Site, Origins: clone(cfg.Origins), Records: map[string][]Receipt{}, Progress: map[string]Progress{}}}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		if err = s.persist(s.state); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		defer f.Close()
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > int64(cfg.MaxBytes) {
			return nil, ErrUnavailable
		}
		b, e := io.ReadAll(io.LimitReader(f, int64(cfg.MaxBytes)+1))
		var env envelope
		if e != nil || authority.DecodeStrictLimit(b, &env, cfg.MaxBytes) != nil || env.SHA256 != hash(env.State) || authority.DecodeStrictLimit(env.State, &s.state, cfg.MaxBytes) != nil {
			return nil, ErrUnavailable
		}
		if s.state.Version != 1 || s.state.Tenant != cfg.Tenant || s.state.Site != cfg.Site || !same(s.state.Origins, cfg.Origins) || s.state.Records == nil || s.state.Progress == nil || s.state.LastUnixMS < 0 || s.state.LastUnixMS > cfg.Now().UnixMilli() {
			return nil, ErrUnavailable
		}
		count := 0
		for origin, rs := range s.state.Records {
			if _, yes := cfg.Origins[origin]; !yes {
				return nil, ErrUnavailable
			}
			previous := zero()
			for i, r := range rs {
				if r.Sequence != uint64(i+1) || s.validate(context.Background(), origin, r, false) != nil || r.PreviousSHA256 != previous || r.Incarnation != s.state.Progress[origin].Incarnation {
					return nil, ErrUnavailable
				}
				previous = tasking.AuditEventHash(previous, event(r))
				count++
			}
			if s.state.Progress[origin].Through != uint64(len(rs)) || s.state.Progress[origin].SHA256 != prefix(rs) {
				return nil, ErrUnavailable
			}
		}
		for origin, p := range s.state.Progress {
			if _, yes := cfg.Origins[origin]; !yes || p.Through != uint64(len(s.state.Records[origin])) || p.LastSyncUnixMS < 0 || p.LastSyncUnixMS > s.state.LastUnixMS {
				return nil, ErrUnavailable
			}
		}
		if count > cfg.MaxEntries {
			return nil, ErrCapacity
		}
	}
	ok = true
	return s, nil
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

// atomicWriteHook is a test seam for child-process crash windows.
var atomicWriteHook func(string)

func writeAtomic(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".task-history-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if atomicWriteHook != nil {
		atomicWriteHook("synced-temp")
	}
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	if atomicWriteHook != nil {
		atomicWriteHook("renamed")
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) MatchesScope(tenant, site string) bool {
	return s != nil && s.cfg.Tenant == tenant && s.cfg.Site == site
}
func (s *Store) Origins() map[string]Origin { return clone(s.cfg.Origins) }
func (s *Store) ready() error {
	if s.closed || s.fault != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Store) validate(ctx context.Context, origin string, r Receipt, current bool) error {
	o, ok := s.cfg.Origins[origin]
	if !ok || r.Audience != origin || r.Assignee != o.Assignee || r.RunID != o.RunID || r.Grant.Grant.Principal != o.Principal {
		return ErrInvalid
	}
	if _, e := s.cfg.Verifier.VerifyTaskAudit(ctx, r, current); e != nil {
		return e
	}
	var a tasking.Audit
	if authority.DecodeStrictLimit(r.Event, &a, authority.MaxTaskStatusRecordBytes) != nil || a.Sequence != r.Sequence || a.UnixMS <= 0 || a.UnixMS > r.ExportedUnixMS || !name(a.Principal) || !name(a.Event) || len(a.TaskID) > 256 {
		return ErrInvalid
	}
	canonical, _ := json.Marshal(a)
	if !bytes.Equal(canonical, r.Event) {
		return ErrInvalid
	}
	if len(a.Proof) > 0 {
		var p authority.SignedInstruction
		if authority.DecodeStrict(a.Proof, &p) != nil || (p.Operation != "mission" && p.Operation != "stop") {
			return ErrInvalid
		}
		id, e := s.cfg.Verifier.VerifyInstructionHistoryForAudience(p, p.Operation, origin)
		if e != nil || id.Principal != a.Principal || p.IssuedAtUnixMS > a.UnixMS {
			return ErrInvalid
		}
		var payload struct {
			ID       string `json:"id"`
			TaskID   string `json:"task_id"`
			Site     string `json:"site"`
			Assignee string `json:"assignee"`
		}
		if json.Unmarshal(p.Payload, &payload) != nil {
			return ErrInvalid
		}
		if p.Operation == "mission" && (payload.ID != a.TaskID || payload.Site != s.cfg.Site || payload.Assignee != o.Assignee) {
			return ErrInvalid
		}
		if p.Operation == "stop" && payload.TaskID != a.TaskID {
			return ErrInvalid
		}
	}
	return nil
}

// currentPage checks cryptographic/current authority, then applies the earliest
// grant deadline once more after all potentially expensive page verification.
func (s *Store) currentPage(ctx context.Context, origin string, rows []Receipt) error {
	for _, r := range rows {
		if e := s.validate(ctx, origin, r, true); e != nil {
			return e
		}
	}
	now := s.cfg.Now().UnixMilli()
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	for _, r := range rows {
		if now < r.Grant.Grant.NotBeforeUnixMS || now >= r.Grant.Grant.NotAfterUnixMS {
			return authority.ErrDenied
		}
	}
	return nil
}
func (s *Store) persist(next state) error {
	return s.persistChecked(next, nil)
}
func (s *Store) persistChecked(next state, check func() error) error {
	raw, e := json.Marshal(next)
	if e != nil {
		return e
	}
	b, e := json.Marshal(envelope{hash(raw), raw})
	if e != nil {
		return e
	}
	if len(b) > s.cfg.MaxBytes {
		return ErrCapacity
	}
	if check != nil {
		if e = check(); e != nil {
			return e
		}
	}
	if e = s.write(s.path, b); e != nil {
		s.fault = e
		return ErrUnavailable
	}
	s.state = next
	return nil
}
func (s *Store) Cursor(origin string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return Request{}, e
	}
	o, ok := s.cfg.Origins[origin]
	if !ok {
		return Request{}, ErrInvalid
	}
	p := s.state.Progress[origin]
	return Request{origin, o.RunID, p.Incarnation, p.Through, prefix(s.state.Records[origin])}, nil
}
func (s *Store) Import(ctx context.Context, p Page, via string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return e
	}
	b, _ := json.Marshal(p)
	o, ok := s.cfg.Origins[p.Origin]
	if !ok || p.RunID != o.RunID || len(p.Incarnation) != 64 || !name(via) || len(b)+1 > MaxPageBytes || len(p.Records) > 16 || p.Through < p.After || p.Through-p.After != uint64(len(p.Records)) || (p.More && len(p.Records) == 0) {
		return ErrInvalid
	}
	old := s.state.Records[p.Origin]
	progress := s.state.Progress[p.Origin]
	if p.After > uint64(len(old)) || (progress.Incarnation != "" && progress.Incarnation != p.Incarnation) || p.PreviousSHA256 != prefix(old[:p.After]) {
		return ErrConflict
	}
	if len(p.Records) == 0 {
		if len(old) == 0 {
			return nil
		}
		if e := s.validate(ctx, p.Origin, old[len(old)-1], true); e != nil {
			return e
		}
	}
	previous := p.PreviousSHA256
	for i, r := range p.Records {
		if r.Sequence != p.After+uint64(i)+1 || r.Incarnation != p.Incarnation || r.PreviousSHA256 != previous || s.validate(ctx, p.Origin, r, true) != nil {
			return ErrInvalid
		}
		previous = tasking.AuditEventHash(previous, event(r))
		if r.Sequence <= uint64(len(old)) && !bytes.Equal(r.Event, old[r.Sequence-1].Event) {
			return ErrConflict
		}
	}
	now := s.cfg.Now().UnixMilli()
	if ctx.Err() != nil || now < s.state.LastUnixMS {
		return ErrUnavailable
	}
	next := clone(s.state)
	for _, r := range p.Records {
		if r.Sequence > uint64(len(next.Records[p.Origin])) {
			next.Records[p.Origin] = append(next.Records[p.Origin], clone(r))
		}
	}
	count := 0
	for _, rs := range next.Records {
		count += len(rs)
	}
	if count > s.cfg.MaxEntries {
		return ErrCapacity
	}
	progress.Through = uint64(len(next.Records[p.Origin]))
	progress.Incarnation = p.Incarnation
	progress.SHA256 = prefix(next.Records[p.Origin])
	progress.LastSyncUnixMS = now
	progress.Via = via
	progress.CaughtUpWithPeer = !p.More && p.Through == progress.Through
	next.Progress[p.Origin] = progress
	next.LastUnixMS = now
	return s.persistChecked(next, func() error {
		if ctx.Err() != nil || s.cfg.Now().UnixMilli() < now {
			return ErrUnavailable
		}
		rows := p.Records
		if len(rows) == 0 {
			rows = old[len(old)-1:]
		}
		// Recheck after cloning/encoding: every attestation must still be current
		// when the atomic write is authorized. Retained history stays unchanged on failure.
		return s.currentPage(ctx, p.Origin, rows)
	})
}
func (s *Store) Export(ctx context.Context, q Request) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Page{Origin: q.Origin, RunID: q.RunID, After: q.After, Through: q.After, Records: []Receipt{}}
	if e := s.ready(); e != nil {
		return p, e
	}
	o, ok := s.cfg.Origins[q.Origin]
	if !ok || q.RunID != o.RunID {
		return p, ErrInvalid
	}
	rs := s.state.Records[q.Origin]
	progress := s.state.Progress[q.Origin]
	if progress.Incarnation == "" || q.After > uint64(len(rs)) || (q.Incarnation != "" && q.Incarnation != progress.Incarnation) || q.PreviousSHA256 != prefix(rs[:q.After]) || (q.After > 0 && q.Incarnation == "") {
		return p, ErrInvalid
	}
	p.Incarnation = progress.Incarnation
	p.PreviousSHA256 = q.PreviousSHA256
	if q.After > 0 {
		if e := s.validate(ctx, q.Origin, rs[q.After-1], true); e != nil {
			return p, e
		}
	}
	for _, r := range rs[q.After:] {
		if len(p.Records) == 16 {
			break
		}
		if e := s.validate(ctx, q.Origin, r, true); e != nil {
			return p, e
		}
		candidate := p
		candidate.Records = append(append([]Receipt{}, p.Records...), clone(r))
		candidate.Through = r.Sequence
		candidate.More = candidate.Through < uint64(len(rs))
		b, _ := json.Marshal(candidate)
		if len(b)+1 > MaxPageBytes {
			break
		}
		p = candidate
	}
	p.More = p.Through < uint64(len(rs))
	if p.More && len(p.Records) == 0 {
		return p, ErrCapacity
	}
	if len(p.Records) > 0 {
		if e := s.currentPage(ctx, q.Origin, p.Records); e != nil {
			return Page{}, e
		}
	} else if q.After > 0 {
		if e := s.currentPage(ctx, q.Origin, rs[q.After-1:q.After]); e != nil {
			return Page{}, e
		}
	}
	return p, nil
}

type listCursor struct {
	Scope       string `json:"scope"`
	Origin      string `json:"origin"`
	Task        string `json:"task"`
	Incarnation string `json:"incarnation"`
	Through     uint64 `json:"through"`
	Prefix      string `json:"prefix"`
}

func (s *Store) List(ctx context.Context, origin, task string, limit int, after string) (List, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := List{Origin: origin, TaskID: task, Historical: true, Events: []View{}}
	if e := s.ready(); e != nil {
		return out, e
	}
	o, ok := s.cfg.Origins[origin]
	if !ok || len(task) > 256 || limit < 1 || limit > 100 || len(after) > 2048 {
		return out, ErrInvalid
	}
	out.RunID = o.RunID
	out.Progress = s.state.Progress[origin]
	config, _ := json.Marshal([]any{s.cfg.Tenant, s.cfg.Site, s.cfg.Origins})
	scope := hash(config)
	rs := s.state.Records[origin]
	start := uint64(0)
	if after != "" {
		b, e := base64.RawURLEncoding.DecodeString(after)
		var c listCursor
		if e != nil || authority.DecodeStrict(b, &c) != nil || c.Scope != scope || c.Origin != origin || c.Task != task || c.Incarnation != out.Progress.Incarnation || c.Through > uint64(len(rs)) || c.Prefix != prefix(rs[:c.Through]) {
			return out, ErrInvalid
		}
		start = c.Through
	}
	for i := start; i < uint64(len(rs)); i++ {
		r := rs[i]
		a := event(r)
		if task != "" && a.TaskID != task {
			continue
		}
		current := "current_origin_grant_verified"
		if _, e := s.cfg.Verifier.VerifyTaskAudit(ctx, r, true); e != nil {
			current = "not_currently_verified"
			if errors.Is(e, authority.ErrStaleRevocations) {
				current = "revocation_state_unavailable"
			} else if s.cfg.Now().UnixMilli() >= r.Grant.Grant.NotAfterUnixMS {
				current = "origin_grant_expired"
			}
		}
		original := "not_present"
		if len(a.Proof) > 0 {
			original = "signature_valid_at_issuance_no_historical_revocation_claim"
		}
		v := View{clone(r), clone(a), true, current, original, false}
		candidate := out
		candidate.Events = append(append([]View{}, out.Events...), v)
		cursor, _ := json.Marshal(listCursor{scope, origin, task, out.Progress.Incarnation, i + 1, prefix(rs[:i+1])})
		candidate.Next = base64.RawURLEncoding.EncodeToString(cursor)
		b, _ := json.Marshal(candidate)
		if len(out.Events) == limit || len(b)+1 > MaxPageBytes {
			if len(out.Events) == 0 {
				return out, ErrCapacity
			}
			return out, nil
		}
		out = candidate
	}
	out.Next = ""
	return out, nil
}
