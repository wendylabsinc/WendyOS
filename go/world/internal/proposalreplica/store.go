// Package proposalreplica retains read-only proposal history. Its records can
// never satisfy the authoritative proposal store used by task admission.
package proposalreplica

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

const MaxPageBytes = 1 << 20
const MaxStoreBytes = 32 << 20

var ErrInvalid = errors.New("invalid proposal history or cursor")
var ErrConflict = errors.New("immutable proposal history conflict")
var ErrCapacity = errors.New("proposal inbox capacity exhausted")
var ErrUnavailable = errors.New("proposal inbox unavailable")

type Origin struct {
	Principal string                         `json:"principal"`
	Assignee  string                         `json:"assignee"`
	RunID     string                         `json:"run_id"`
	Catalog   map[string]mission.SafetyClass `json:"catalog"`
}
type Config struct {
	Tenant, Site         string
	Origins              map[string]Origin
	Verifier             *authority.Verifier
	MaxEntries, MaxBytes int
	Now                  func() time.Time
}
type Receipt struct {
	Record      proposals.Record        `json:"record"`
	OriginProof authority.SignedRequest `json:"origin_proof"`
}
type Page struct {
	Origin  string    `json:"origin"`
	After   uint64    `json:"after"`
	Through uint64    `json:"through"`
	More    bool      `json:"more"`
	Records []Receipt `json:"records"`
}
type Progress struct {
	Through          uint64 `json:"through"`
	LastSyncUnixMS   int64  `json:"last_sync_unix_ms"`
	Via              string `json:"via"`
	CaughtUpWithPeer bool   `json:"caught_up_with_peer"`
}
type View struct {
	Origin                       string                  `json:"origin"`
	Record                       proposals.Record        `json:"record"`
	OriginProof                  authority.SignedRequest `json:"origin_proof"`
	Expired                      bool                    `json:"expired"`
	HistoricalProvenanceVerified bool                    `json:"historical_provenance_verified"`
	CurrentVerification          string                  `json:"current_verification"`
	OriginVerification           string                  `json:"origin_verification"`
	ProposerVerification         string                  `json:"proposer_verification"`
	Executable                   bool                    `json:"executable"`
	Progress                     Progress                `json:"progress"`
}
type List struct {
	Origin    string   `json:"origin"`
	Proposals []View   `json:"proposals"`
	Next      string   `json:"next"`
	Progress  Progress `json:"progress"`
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

func clone[T any](v T) T   { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func same(a, b any) bool   { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func name(v string) bool   { return proposals.ValidID(v) }
func ValidateConfig(cfg Config) error {
	if cfg.Tenant == "" || cfg.Site == "" || len(cfg.Origins) == 0 || len(cfg.Origins) > 8 {
		return ErrInvalid
	}
	for audience, o := range cfg.Origins {
		if !name(audience) || o.Principal == "" || len(o.Principal) > 256 || o.Assignee == "" || len(o.Assignee) > 256 || !name(o.RunID) || len(o.Catalog) == 0 || len(o.Catalog) > 100 {
			return ErrInvalid
		}
		for typ, c := range o.Catalog {
			if typ == "" || len(typ) > 256 || (c != mission.Observe && c != mission.Move && c != mission.Actuate) {
				return ErrInvalid
			}
		}
	}
	if cfg.MaxEntries < 1 || cfg.MaxEntries > 10000 || cfg.MaxBytes < proposals.MaxRecordBytes*2 || cfg.MaxBytes > MaxStoreBytes {
		return ErrInvalid
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
			for i, r := range rs {
				if r.Record.Sequence != uint64(i+1) || s.validate(context.Background(), origin, r, false) != nil {
					return nil, ErrUnavailable
				}
				count++
			}
			if s.state.Progress[origin].Through != uint64(len(rs)) {
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
	if !ok {
		return ErrInvalid
	}
	cfg := proposals.Config{Tenant: s.cfg.Tenant, Site: s.cfg.Site, RunID: o.RunID, Audience: origin, Assignee: o.Assignee, Catalog: o.Catalog}
	if proposals.ValidateHistory(r.Record, cfg) != nil {
		return ErrInvalid
	}
	if _, err := s.cfg.Verifier.VerifyProposalHistory(ctx, r.Record.Proof, "propose", origin, r.Record.Body); err != nil {
		return err
	}
	body, _ := ReceiptBody(origin, r.Record)
	id, err := s.cfg.Verifier.VerifyProposalHistory(ctx, r.OriginProof, "replicate", origin, body)
	if err != nil || id.Principal != o.Principal || r.OriginProof.IssuedAtUnixMS < r.Record.AcceptedAtUnixMS {
		return ErrInvalid
	}
	if current {
		if _, err = s.cfg.Verifier.VerifyGrantContext(ctx, r.OriginProof.Grant, "replicate"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) persist(next state) error {
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
	if e = s.write(s.path, b); e != nil {
		s.fault = e
		return ErrUnavailable
	}
	s.state = next
	return nil
}
func (s *Store) Cursor(origin string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return 0, e
	}
	if _, ok := s.cfg.Origins[origin]; !ok {
		return 0, ErrInvalid
	}
	return s.state.Progress[origin].Through, nil
}

// Import commits a whole contiguous page and its cursor together. Immutable
// duplicates never replace original proofs or reset proposal deadlines.
func (s *Store) Import(ctx context.Context, p Page, via string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return e
	}
	raw, e := json.Marshal(p)
	if e != nil || len(raw) > MaxPageBytes || !name(via) || len(p.Records) > 16 || p.Through < p.After || p.Through-p.After != uint64(len(p.Records)) || (p.More && len(p.Records) == 0) {
		return ErrInvalid
	}
	if _, ok := s.cfg.Origins[p.Origin]; !ok {
		return ErrInvalid
	}
	old := s.state.Records[p.Origin]
	if p.After > uint64(len(old)) {
		return ErrInvalid
	}
	for j, r := range p.Records {
		if r.Record.Sequence != p.After+uint64(j)+1 || s.validate(ctx, p.Origin, r, true) != nil {
			return ErrInvalid
		}
		seq := r.Record.Sequence
		if seq <= uint64(len(old)) {
			if !same(old[seq-1].Record, r.Record) {
				return ErrConflict
			}
		} else {
			for _, prior := range old {
				if prior.Record.Proposal.ID == r.Record.Proposal.ID {
					return ErrConflict
				}
			}
		}
		for k := 0; k < j; k++ {
			if p.Records[k].Record.Proposal.ID == r.Record.Proposal.ID {
				return ErrConflict
			}
		}
	}
	now := s.cfg.Now().UnixMilli()
	if ctx.Err() != nil || now < s.state.LastUnixMS {
		return ErrUnavailable
	}
	next := clone(s.state)
	for _, r := range p.Records {
		if r.Record.Sequence > uint64(len(next.Records[p.Origin])) {
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
	// A lagging peer cannot claim that our newer local prefix is caught up.
	progress := next.Progress[p.Origin]
	progress.Through = uint64(len(next.Records[p.Origin]))
	progress.LastSyncUnixMS = now
	progress.Via = via
	progress.CaughtUpWithPeer = !p.More && p.Through == progress.Through
	next.Progress[p.Origin] = progress
	next.LastUnixMS = now
	return s.persist(next)
}
func (s *Store) Export(ctx context.Context, origin string, after uint64) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Page{Origin: origin, After: after, Through: after, Records: []Receipt{}}
	if e := s.ready(); e != nil {
		return p, e
	}
	if _, ok := s.cfg.Origins[origin]; !ok {
		return p, ErrInvalid
	}
	rs := s.state.Records[origin]
	if after > uint64(len(rs)) {
		return p, ErrInvalid
	}
	for _, r := range rs[after:] {
		if e := s.validate(ctx, origin, r, true); e != nil {
			return p, e
		}
		if len(p.Records) == 16 {
			break
		}
		candidate := p
		candidate.Records = append(append([]Receipt{}, p.Records...), clone(r))
		candidate.Through = r.Record.Sequence
		candidate.More = candidate.Through < uint64(len(rs))
		b, _ := json.Marshal(candidate)
		if len(b)+1 > MaxPageBytes {
			break
		}
		p = candidate
	}
	p.More = p.Through < uint64(len(rs))
	return p, nil
}
func (s *Store) currentGrant(ctx context.Context, g authority.SignedGrant, operation string) string {
	_, e := s.cfg.Verifier.VerifyGrantContext(ctx, g, operation)
	if e == nil {
		return "current_grant_verified"
	}
	if errors.Is(e, authority.ErrStaleRevocations) {
		return "revocation_state_unavailable"
	}
	if s.cfg.Now().UnixMilli() >= g.Grant.NotAfterUnixMS {
		return "grant_expired"
	}
	return "not_currently_verified"
}
func (s *Store) view(ctx context.Context, origin string, r Receipt) View {
	originState := s.currentGrant(ctx, r.OriginProof.Grant, "replicate")
	proposerState := s.currentGrant(ctx, r.Record.Proof.Grant, "propose")
	state := "not_currently_verified"
	if originState == "current_grant_verified" && proposerState == "current_grant_verified" {
		state = "current_grants_verified"
	}
	return View{Origin: origin, Record: clone(r.Record), OriginProof: clone(r.OriginProof), Expired: s.cfg.Now().UnixMilli() >= r.Record.Proposal.NotAfterUnixMS, HistoricalProvenanceVerified: true, CurrentVerification: state, OriginVerification: originState, ProposerVerification: proposerState, Executable: false, Progress: s.state.Progress[origin]}
}

func (s *Store) List(ctx context.Context, origin string, limit int, after string) (List, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := List{Origin: origin, Proposals: []View{}}
	if e := s.ready(); e != nil {
		return out, e
	}
	if _, ok := s.cfg.Origins[origin]; !ok || limit < 1 || limit > 100 || (after != "" && !name(after)) {
		return out, ErrInvalid
	}
	out.Progress = s.state.Progress[origin]
	rs := clone(s.state.Records[origin])
	sort.Slice(rs, func(i, j int) bool { return rs[i].Record.Proposal.ID < rs[j].Record.Proposal.ID })
	for _, r := range rs {
		if r.Record.Proposal.ID <= after {
			continue
		}
		v := s.view(ctx, origin, r)
		candidate := out
		candidate.Proposals = append(append([]View{}, out.Proposals...), v)
		candidate.Next = r.Record.Proposal.ID
		b, _ := json.Marshal(candidate)
		if len(out.Proposals) == limit || len(b)+1 > MaxPageBytes {
			if len(out.Proposals) == 0 {
				return out, ErrCapacity
			}
			out.Next = out.Proposals[len(out.Proposals)-1].Record.Proposal.ID
			return out, nil
		}
		out.Proposals = append(out.Proposals, v)
	}
	return out, nil
}
func (s *Store) Get(ctx context.Context, origin, id string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return View{}, e
	}
	if _, ok := s.cfg.Origins[origin]; !ok || !name(id) {
		return View{}, ErrInvalid
	}
	for _, r := range s.state.Records[origin] {
		if r.Record.Proposal.ID == id {
			return s.view(ctx, origin, r), nil
		}
	}
	return View{}, proposals.ErrNotFound
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
	f, e := os.CreateTemp(filepath.Dir(path), ".proposal-inbox-")
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

// ReceiptBody purpose-binds the origin attestation. This envelope is never a live replica payload.
func ReceiptBody(origin string, r proposals.Record) ([]byte, error) {
	return json.Marshal(struct {
		Purpose string           `json:"purpose"`
		Origin  string           `json:"origin"`
		Record  proposals.Record `json:"record"`
	}{"wendy.world.proposal-history.v1", origin, r})
}
