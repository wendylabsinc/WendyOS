// Package taskrelay keeps signed mission delivery and last-known assignee state.
// It never executes a task or turns a transport acknowledgement into acceptance.
package taskrelay

import (
	"bytes"
	"context"
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

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const MaxStoreBytes = 8 << 20

type Route struct {
	Assignee        string `json:"assignee"`
	StatusPrincipal string `json:"status_principal"`
}
type Config struct {
	Tenant, Site, Domain string
	Routes               map[string]Route // Final audience to its one configured assignee.
	Verifier             *authority.Verifier
	Now                  func() time.Time
	MaxEntries, MaxBytes int
}
type Entry struct {
	Audience string                       `json:"audience"`
	TaskID   string                       `json:"task_id"`
	Mission  *authority.SignedInstruction `json:"mission,omitempty"`
	Stop     *authority.SignedInstruction `json:"stop,omitempty"`
	Status   *authority.SignedTaskStatus  `json:"status,omitempty"`
}
type state struct {
	Version              int `json:"version"`
	Tenant, Site, Domain string
	Routes               map[string]Route `json:"routes"`
	LastUnixMS           int64            `json:"last_unix_ms"`
	Entries              []Entry          `json:"entries"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}
type Store struct {
	mu       sync.Mutex
	cfg      Config
	path     string
	lock     *os.File
	state    state
	closed   bool
	fault    error
	lastWall int64
	write    func(string, []byte) error
}

func validName(s string) bool        { return s != "" && len(s) <= 256 && !strings.ContainsRune(s, '\x00') }
func key(audience, id string) string { return audience + "\x00" + id }
func clone[T any](value T) T {
	b, _ := json.Marshal(value)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func same(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }

func Open(path string, cfg Config) (*Store, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = 1024
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = MaxStoreBytes
	}
	if path == "" || !validName(cfg.Tenant) || !validName(cfg.Site) || cfg.Domain != "simulation" || cfg.Verifier == nil || len(cfg.Routes) == 0 || len(cfg.Routes) > 64 || cfg.MaxEntries < 1 || cfg.MaxEntries > 10000 || cfg.MaxBytes < 4096 || cfg.MaxBytes > MaxStoreBytes {
		return nil, errors.New("invalid relay configuration")
	}
	for audience, route := range cfg.Routes {
		if !validName(audience) || !validName(route.Assignee) || !validName(route.StatusPrincipal) {
			return nil, errors.New("invalid relay route")
		}
	}
	cfg.Routes = clone(cfg.Routes)
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, path: filepath.Join(dir, filepath.Base(absolute)), write: writeAtomic}
	for _, p := range []string{s.path, s.path + ".lock"} {
		if st, e := os.Lstat(p); e == nil && !st.Mode().IsRegular() {
			return nil, errors.New("relay storage must be regular files")
		}
	}
	s.lock, err = os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		s.lock.Close()
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	s.state = state{Version: 1, Tenant: cfg.Tenant, Site: cfg.Site, Domain: cfg.Domain, Routes: clone(cfg.Routes), Entries: []Entry{}}
	f, err := os.OpenFile(s.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, e
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			f.Close()
			return nil, errors.New("relay state must be private regular file")
		}
		data, e := io.ReadAll(io.LimitReader(f, int64(cfg.MaxBytes)+1))
		f.Close()
		if e != nil {
			return nil, e
		}
		if len(data) > cfg.MaxBytes {
			return nil, errors.New("relay state too large")
		}
		var saved envelope
		if e = authority.DecodeStrictLimit(data, &saved, cfg.MaxBytes); e != nil {
			return nil, e
		}
		hash := sha256.Sum256(saved.State)
		if saved.SHA256 != hex.EncodeToString(hash[:]) {
			return nil, errors.New("relay checksum mismatch")
		}
		if e = authority.DecodeStrictLimit(saved.State, &s.state, cfg.MaxBytes); e != nil {
			return nil, e
		}
		if s.state.Version != 1 || s.state.Tenant != cfg.Tenant || s.state.Site != cfg.Site || s.state.Domain != cfg.Domain || !same(s.state.Routes, cfg.Routes) || s.state.Entries == nil || len(s.state.Entries) > cfg.MaxEntries {
			return nil, errors.New("relay state scope or bounds mismatch")
		}
		seen := map[string]bool{}
		for _, entry := range s.state.Entries {
			k := key(entry.Audience, entry.TaskID)
			if seen[k] {
				return nil, errors.New("duplicate persisted relay entry")
			}
			seen[k] = true
			if e = s.validateStored(entry); e != nil {
				return nil, e
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.lastWall = s.state.LastUnixMS
	if err = s.commit(clone(s.state)); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *Store) clock() (int64, error) {
	if s.closed {
		return 0, errors.New("relay store closed")
	}
	if s.fault != nil {
		return 0, fmt.Errorf("relay store fault; reopen required: %w", s.fault)
	}
	now := s.cfg.Now().UnixMilli()
	if now <= 0 || now < s.lastWall || now < s.state.LastUnixMS {
		s.fault = errors.New("relay clock moved backwards")
		return 0, s.fault
	}
	s.lastWall = now
	return now, nil
}
func (s *Store) commit(next state) error {
	now, err := s.clock()
	if err != nil {
		return err
	}
	next.LastUnixMS = now
	sort.Slice(next.Entries, func(i, j int) bool {
		return key(next.Entries[i].Audience, next.Entries[i].TaskID) < key(next.Entries[j].Audience, next.Entries[j].TaskID)
	})
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	data, err := json.Marshal(envelope{hex.EncodeToString(h[:]), b})
	if err != nil {
		return err
	}
	if len(next.Entries) > s.cfg.MaxEntries || len(data) > s.cfg.MaxBytes {
		return errors.New("relay storage quota exceeded")
	}
	if err = s.write(s.path, data); err != nil {
		s.fault = err
		return err
	}
	s.state = next
	return nil
}
func (s *Store) index(audience, id string) int {
	for i, e := range s.state.Entries {
		if e.Audience == audience && e.TaskID == id {
			return i
		}
	}
	return -1
}
func (s *Store) replace(entry Entry) error {
	next := clone(s.state)
	index := s.index(entry.Audience, entry.TaskID)
	if index < 0 {
		next.Entries = append(next.Entries, clone(entry))
	} else {
		next.Entries[index] = clone(entry)
	}
	return s.commit(next)
}

// PutInstruction validates live authority before durably queuing original proof.
// A retained stop blocks dispatch even if its delivery instruction later expires.
func (s *Store) PutInstruction(in authority.SignedInstruction) (Entry, error) {
	return s.PutInstructionContext(context.Background(), in)
}
func (s *Store) PutInstructionContext(ctx context.Context, in authority.SignedInstruction) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.clock(); err != nil {
		return Entry{}, err
	}
	id, err := s.instructionContext(ctx, in, false)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{Audience: in.Audience, TaskID: id}
	if index := s.index(in.Audience, id); index >= 0 {
		entry = clone(s.state.Entries[index])
	}
	if in.Operation == "mission" {
		// The assignee's authenticated status is authoritative over any queued request.
		if entry.Status != nil {
			var accepted tasking.Record
			var proposed tasking.Spec
			_ = authority.DecodeStrictLimit(entry.Status.Record, &accepted, authority.MaxTaskStatusRecordBytes)
			_ = authority.DecodeStrict(in.Payload, &proposed)
			if !same(accepted.Spec, proposed) {
				return Entry{}, errors.New("task ID bound to different assignee state")
			}
			return clone(entry), nil
		}
		if entry.Mission != nil {
			var oldSpec, newSpec tasking.Spec
			_ = authority.DecodeStrict(entry.Mission.Payload, &oldSpec)
			_ = authority.DecodeStrict(in.Payload, &newSpec)
			if !same(oldSpec, newSpec) {
				return Entry{}, errors.New("task ID bound to different queued mission")
			}
			if _, err := s.instructionContext(ctx, *entry.Mission, false); err != nil {
				return Entry{}, errors.New("original queued authority no longer valid; use a new task ID")
			}
			return clone(entry), nil
		}
		copied := clone(in)
		entry.Mission = &copied
	} else {
		if entry.Stop != nil {
			// Keep the stop with the longest remaining delivery window. The latch never clears.
			_, oldValid := s.instructionContext(ctx, *entry.Stop, false)
			if oldValid == nil && (in.ExpiresAtUnixMS < entry.Stop.ExpiresAtUnixMS || (in.ExpiresAtUnixMS == entry.Stop.ExpiresAtUnixMS && in.IssuedAtUnixMS <= entry.Stop.IssuedAtUnixMS)) {
				return clone(entry), nil
			}
		}
		copied := clone(in)
		entry.Stop = &copied
	}
	if err = s.replace(entry); err != nil {
		return Entry{}, err
	}
	return clone(entry), nil
}

// PutStatus stores only state signed by the route's assignee device. It can
// repair a conflicting local queue, which never had authority to accept a task.
func (s *Store) PutStatus(in authority.SignedTaskStatus) (Entry, error) {
	return s.PutStatusContext(context.Background(), in)
}
func (s *Store) PutStatusContext(ctx context.Context, in authority.SignedTaskStatus) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.clock(); err != nil {
		return Entry{}, err
	}
	if err := s.statusContext(ctx, in, true); err != nil {
		return Entry{}, err
	}
	entry := Entry{Audience: in.Audience, TaskID: in.TaskID}
	if index := s.index(in.Audience, in.TaskID); index >= 0 {
		entry = clone(s.state.Entries[index])
	}
	if old := entry.Status; old != nil {
		if in.StatusVersion < old.StatusVersion {
			return clone(entry), nil
		}
		var a, b tasking.Record
		_ = authority.DecodeStrictLimit(old.Record, &a, authority.MaxTaskStatusRecordBytes)
		_ = authority.DecodeStrictLimit(in.Record, &b, authority.MaxTaskStatusRecordBytes)
		if old.Terminal && !same(a, b) {
			return Entry{}, errors.New("terminal assignee state is immutable")
		}
		if in.StatusVersion == old.StatusVersion {
			if !same(a, b) {
				return Entry{}, errors.New("conflicting assignee status version")
			}
			if in.SignedAtUnixMS <= old.SignedAtUnixMS {
				return clone(entry), nil
			}
		}
		if !same(a.Spec, b.Spec) || !same(a.Proof, b.Proof) || a.AuthorizationUntilUnixMS != b.AuthorizationUntilUnixMS {
			return Entry{}, errors.New("assignee changed immutable mission")
		}
	}
	copy := clone(in)
	entry.Status = &copy
	if err := s.replace(entry); err != nil {
		return Entry{}, err
	}
	return clone(entry), nil
}

func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.clock(); err != nil {
		return nil, err
	}
	return clone(s.state.Entries), nil
}

// Export includes currently verifiable delivery instructions and status proofs.
// Expired queued instructions stay in List as history but cannot be delivered.
func (s *Store) Export() ([]Entry, error) {
	return s.ExportContext(context.Background())
}
func (s *Store) ExportContext(ctx context.Context) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.clock(); err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, original := range s.state.Entries {
		e := Entry{Audience: original.Audience, TaskID: original.TaskID}
		if original.Stop != nil {
			if _, err := s.instructionContext(ctx, *original.Stop, false); err == nil {
				e.Stop = original.Stop
			}
		}
		if original.Mission != nil && original.Stop == nil && original.Status == nil {
			if _, err := s.instructionContext(ctx, *original.Mission, false); err == nil {
				e.Mission = original.Mission
			}
		}
		if original.Status != nil && s.statusContext(ctx, *original.Status, true) == nil {
			e.Status = original.Status
		}
		if e.Mission != nil || e.Stop != nil || e.Status != nil {
			out = append(out, clone(e))
		}
	}
	return out, nil
}

// Pending returns stops first. Any known stop or assignee state suppresses
// mission redelivery; the assignee's local durable engine remains authoritative.
func (s *Store) Pending(audience string) ([]authority.SignedInstruction, error) {
	return s.PendingContext(context.Background(), audience)
}
func (s *Store) PendingContext(ctx context.Context, audience string) ([]authority.SignedInstruction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.clock(); err != nil {
		return nil, err
	}
	if _, ok := s.cfg.Routes[audience]; !ok {
		return nil, errors.New("unknown destination")
	}
	stops, missions := []authority.SignedInstruction{}, []authority.SignedInstruction{}
	for _, e := range s.state.Entries {
		if e.Audience != audience {
			continue
		}
		if e.Stop != nil {
			if e.Status != nil && e.Status.Terminal {
				continue
			}
			if _, err := s.instructionContext(ctx, *e.Stop, false); err == nil {
				stops = append(stops, clone(*e.Stop))
			}
			continue
		}
		if e.Mission != nil && e.Status == nil {
			if _, err := s.instructionContext(ctx, *e.Mission, false); err == nil {
				missions = append(missions, clone(*e.Mission))
			}
		}
	}
	return append(stops, missions...), nil
}

func (s *Store) instruction(in authority.SignedInstruction, history bool) (string, error) {
	return s.instructionContext(context.Background(), in, history)
}
func (s *Store) instructionContext(ctx context.Context, in authority.SignedInstruction, history bool) (string, error) {
	route, ok := s.cfg.Routes[in.Audience]
	if !ok || in.Grant.Grant.Tenant != s.cfg.Tenant || in.Grant.Grant.Site != s.cfg.Site || in.Grant.Grant.Domain != s.cfg.Domain {
		return "", errors.New("instruction outside relay scope")
	}
	var err error
	if history {
		_, err = s.cfg.Verifier.VerifyInstructionHistoryForAudience(in, in.Operation, in.Audience)
	} else {
		_, err = s.cfg.Verifier.VerifyInstructionForAudienceContext(ctx, in, in.Operation, in.Audience)
	}
	if err != nil {
		return "", err
	}
	if in.Operation == "mission" {
		var spec tasking.Spec
		if err = authority.DecodeStrict(in.Payload, &spec); err != nil {
			return "", err
		}
		if !validName(spec.ID) || spec.Assignee != route.Assignee || spec.Site != s.cfg.Site || string(spec.Domain) != s.cfg.Domain || spec.NotAfterUnixMS <= in.IssuedAtUnixMS || spec.NotAfterUnixMS > in.ExpiresAtUnixMS || spec.NotAfterUnixMS > in.Grant.Grant.NotAfterUnixMS {
			return "", errors.New("mission outside signed route or lifetime")
		}
		if !history && s.cfg.Now().UnixMilli() >= spec.NotAfterUnixMS {
			return "", errors.New("queued mission deadline expired")
		}
		if err = spec.Frame.Validate(); err != nil {
			return "", err
		}
		return spec.ID, nil
	}
	if in.Operation == "stop" {
		var stop struct {
			TaskID string `json:"task_id"`
		}
		if err = authority.DecodeStrict(in.Payload, &stop); err != nil || !validName(stop.TaskID) {
			return "", errors.New("invalid relay stop")
		}
		return stop.TaskID, nil
	}
	return "", errors.New("unsupported relay operation")
}

func (s *Store) status(in authority.SignedTaskStatus, current bool) error {
	return s.statusContext(context.Background(), in, current)
}
func (s *Store) statusContext(ctx context.Context, in authority.SignedTaskStatus, current bool) error {
	route, ok := s.cfg.Routes[in.Audience]
	if !ok || !validName(in.TaskID) || in.Assignee != route.Assignee || in.Grant.Grant.Principal != route.StatusPrincipal || in.Grant.Grant.Tenant != s.cfg.Tenant || in.Grant.Grant.Site != s.cfg.Site || in.Grant.Grant.Domain != s.cfg.Domain {
		return errors.New("status outside configured assignee")
	}
	// Persisted state is checked structurally and against operator history on open.
	// Live import/export additionally requires a non-revoked gateway signature.
	if current {
		if _, err := s.cfg.Verifier.VerifyTaskStatusContext(ctx, in); err != nil {
			return err
		}
	} else if _, err := s.cfg.Verifier.VerifyTaskStatusHistory(in); err != nil {
		return err
	}
	var record tasking.Record
	if err := authority.DecodeStrictLimit(in.Record, &record, authority.MaxTaskStatusRecordBytes); err != nil {
		return err
	}
	if record.Spec.ID != in.TaskID || record.Spec.Assignee != route.Assignee || record.Spec.Site != s.cfg.Site || string(record.Spec.Domain) != s.cfg.Domain || record.Status.Version != in.StatusVersion || in.StatusVersion == 0 || record.Status.Phase.Terminal() != in.Terminal || len(record.Status.Reason) > 4096 {
		return errors.New("status record differs from envelope")
	}
	switch record.Status.Phase {
	case mission.Accepted, mission.Running, mission.Held, mission.Returning, mission.Succeeded, mission.Failed, mission.Canceled, mission.Rejected:
	default:
		return errors.New("unknown assignee phase")
	}
	var proof authority.SignedInstruction
	if err := authority.DecodeStrict(record.Proof, &proof); err != nil {
		return err
	}
	id, err := s.instructionContext(ctx, proof, true)
	if err != nil {
		return err
	}
	if proof.Audience != in.Audience || id != in.TaskID || in.SignedAtUnixMS < proof.IssuedAtUnixMS || record.AuthorizationUntilUnixMS <= proof.IssuedAtUnixMS || record.AuthorizationUntilUnixMS > proof.ExpiresAtUnixMS {
		return errors.New("status admission proof mismatch")
	}
	if proof.Operation == "mission" {
		var spec tasking.Spec
		_ = authority.DecodeStrict(proof.Payload, &spec)
		if !same(spec, record.Spec) || record.AuthorizationUntilUnixMS > proof.Grant.Grant.NotAfterUnixMS {
			return errors.New("status changed original mission")
		}
	} else {
		expected := tasking.Spec{ID: in.TaskID, Site: s.cfg.Site, Assignee: route.Assignee, Domain: mission.Domain(s.cfg.Domain)}
		if record.Status.Phase != mission.Canceled || record.Status.Version != 1 || record.Result != nil || !same(record.Spec, expected) {
			return errors.New("invalid stop-before-task state")
		}
	}
	if record.Result != nil && (record.Spec.Type != tasking.InspectType || record.Status.Phase != mission.Succeeded) {
		return errors.New("unexpected task result")
	}
	if record.Spec.Type == tasking.InspectType && record.Status.Phase == mission.Succeeded {
		result := record.Result
		if result == nil || result.TaskID != in.TaskID || result.Frame != record.Spec.Frame || result.RunID == "" || result.CapturedUnixMS < proof.IssuedAtUnixMS || result.CapturedUnixMS > in.SignedAtUnixMS || result.SettledSinceUnixMS <= 0 || result.SettledSinceUnixMS > result.CapturedUnixMS || result.SettledSinceSimulationNS < 0 || result.SettledSinceSimulationNS > result.SimulationTimeNS || result.Sequence == 0 || result.BootID == "" || result.Session == "" || !same(result.Trigger, record.Spec.Trigger) || len(result.Samples) == 0 || len(result.Samples) > 64 {
			return errors.New("inspection result identity or capture binding mismatch")
		}
		for _, sample := range result.Samples {
			if err := sample.Validate(); err != nil {
				return err
			}
			if sample.SiteID != s.cfg.Site || sample.Frame != record.Spec.Frame || sample.RunID != result.RunID || sample.Sequence != result.Sequence || sample.SampleNS != result.SimulationTimeNS {
				return errors.New("inspection sample differs from result context")
			}
		}
	}
	return nil
}
func (s *Store) validateStored(entry Entry) error {
	if !validName(entry.Audience) || !validName(entry.TaskID) || (entry.Mission == nil && entry.Stop == nil && entry.Status == nil) {
		return errors.New("invalid persisted relay entry")
	}
	for _, i := range []*authority.SignedInstruction{entry.Mission, entry.Stop} {
		if i == nil {
			continue
		}
		id, err := s.instruction(*i, true)
		if err != nil {
			return err
		}
		if i.Audience != entry.Audience || id != entry.TaskID {
			return errors.New("persisted instruction key mismatch")
		}
	}
	if entry.Mission != nil && entry.Mission.Operation != "mission" {
		return errors.New("invalid mission slot")
	}
	if entry.Stop != nil && entry.Stop.Operation != "stop" {
		return errors.New("invalid stop slot")
	}
	if entry.Status != nil {
		if entry.Status.Audience != entry.Audience || entry.Status.TaskID != entry.TaskID {
			return errors.New("persisted status key mismatch")
		}
		return s.status(*entry.Status, false)
	}
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
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".task-relay-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
