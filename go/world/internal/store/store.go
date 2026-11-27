// Package store provides bounded, durable local world replicas. It assumes a
// trusted local filesystem and a clock whose domain survives process restarts.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const maxFileBytes = 64 << 20

var ErrClosed = errors.New("store is closed")
var ErrLocked = errors.New("store is already open")
var ErrFaulted = errors.New("store requires reopen after persistence failure")

type epoch struct {
	Source world.Source `json:"source"`
	Value  uint64       `json:"value"`
}
type state struct {
	Subscription *subscription.Policy `json:"subscription,omitempty"`
	Version      int                  `json:"version"`
	World        world.Checkpoint     `json:"world"`
	Epochs       []epoch              `json:"epochs"`
}
type envelope struct {
	SHA256 string          `json:"sha256"`
	State  json.RawMessage `json:"state"`
}

// Store serializes reads and writes; every successful operation checkpoints its
// observed clock. A mutation becomes visible only after durable replacement.
type Store struct {
	subscription *subscription.Policy
	mu           sync.Mutex
	path         string
	config       world.Config
	replica      *world.Replica
	epochs       map[world.Source]uint64
	lock         *os.File
	closed       bool
	fault        error
	write        func(string, []byte) error
	metrics      *runtimeobs.Collector
}

// SetMetrics enables bounded process-local diagnostics. Nil disables collection.
// It does not change persistence or replay behavior, and records no private data.
func (s *Store) SetMetrics(metrics *runtimeobs.Collector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = metrics
}

func Open(path string, config world.Config) (*Store, error) {
	return OpenSubscribed(path, config, nil)
}

// OpenSubscribed binds a new store to an immutable scope. Existing full stores
// are never silently converted, and omitting a retained policy fails closed.
func OpenSubscribed(path string, config world.Config, policy *subscription.Policy) (*Store, error) {
	if policy != nil {
		normalized, err := policy.Normalize()
		if err != nil {
			return nil, err
		}
		if normalized.Tenant != config.Tenant || normalized.Site != config.Site {
			return nil, subscription.ErrMismatch
		}
		policy = &normalized
	}
	if path == "" {
		return nil, errors.New("store path required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(dir, filepath.Base(abs))
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("store file must not be a symlink")
	}
	lock, err := os.OpenFile(abs+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("%w: %v", ErrLocked, err)
	}
	s := &Store{subscription: policy, path: abs, config: config, epochs: map[world.Source]uint64{}, lock: lock, write: writeAtomic}
	success := false
	defer func() {
		if !success {
			s.Close()
		}
	}()
	f, err := os.Open(abs)
	if errors.Is(err, os.ErrNotExist) {
		s.replica, err = world.NewReplica(config)
	} else if err == nil {
		data, readErr := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > maxFileBytes {
			return nil, errors.New("store exceeds size limit")
		}
		var e envelope
		if err := decode(data, &e); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(e.State)
		if e.SHA256 != hex.EncodeToString(digest[:]) {
			return nil, errors.New("store checksum mismatch")
		}
		var saved state
		if err := decode(e.State, &saved); err != nil {
			return nil, err
		}
		if (saved.Version != 1 && saved.Version != 2) || (saved.Version == 1 && saved.Subscription != nil) || (saved.Version == 2 && saved.Subscription == nil) {
			return nil, errors.New("unsupported store version or missing subscription")
		}
		if saved.Subscription != nil {
			normalized, e := saved.Subscription.Normalize()
			if e != nil {
				return nil, e
			}
			saved.Subscription = &normalized
		}
		if !subscription.Equal(policy, saved.Subscription) {
			return nil, subscription.ErrMismatch
		}
		for _, entry := range saved.World.Entries {
			if !policy.Retains(entry.Component) {
				return nil, subscription.ErrExcluded
			}
		}
		for _, f := range saved.World.Fences {
			if !policy.Regional() || !policy.Allows(f.Key) {
				return nil, subscription.ErrMismatch
			}
		}
		s.replica, err = world.Restore(config, saved.World)
		if err != nil {
			return nil, err
		}
		if len(saved.Epochs) > config.MaxEntries {
			return nil, errors.New("epoch capacity exceeded")
		}
		for _, e := range saved.Epochs {
			if err := e.Source.Validate(); err != nil {
				return nil, err
			}
			if e.Value == 0 {
				return nil, errors.New("invalid epoch")
			}
			if _, exists := s.epochs[e.Source]; exists {
				return nil, errors.New("duplicate epoch source")
			}
			s.epochs[e.Source] = e.Value
		}
	}
	if err != nil {
		return nil, err
	}
	if err := s.persist(s.replica, s.epochs); err != nil {
		return nil, err
	}
	success = true
	return s, nil
}

func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing store data")
	}
	return nil
}

func (s *Store) check() error {
	if s.closed {
		return ErrClosed
	}
	if s.fault != nil {
		return fmt.Errorf("%w: %v", ErrFaulted, s.fault)
	}
	return nil
}

func (s *Store) candidate() (*world.Replica, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.replica.Clone()
}

type preparedState struct {
	replica     *world.Replica
	checkpoint  world.Checkpoint
	epochs      map[world.Source]uint64
	data        []byte
	preparation time.Duration
}

func (s *Store) prepare(r *world.Replica, epochs map[world.Source]uint64) (*preparedState, error) {
	var started time.Time
	if s.metrics != nil {
		started = time.Now()
	}
	cp, err := r.Checkpoint()
	if err != nil {
		return nil, err
	}
	saved := state{Version: 1, World: cp, Epochs: []epoch{}}
	if s.subscription != nil {
		saved.Version = 2
		saved.Subscription = s.subscription
		for _, entry := range cp.Entries {
			if !s.subscription.Retains(entry.Component) {
				return nil, subscription.ErrExcluded
			}
		}
	}
	for _, f := range cp.Fences {
		if !s.subscription.Regional() || !s.subscription.Allows(f.Key) {
			return nil, subscription.ErrMismatch
		}
	}
	// Marshal each sort key once while retaining the original source values.
	type keyedEpoch struct {
		key   []byte
		epoch epoch
	}
	keyed := make([]keyedEpoch, 0, len(epochs))
	for source, value := range epochs {
		key, err := json.Marshal(source)
		if err != nil {
			return nil, err
		}
		keyed = append(keyed, keyedEpoch{key: key, epoch: epoch{source, value}})
	}
	sort.Slice(keyed, func(i, j int) bool {
		return bytes.Compare(keyed[i].key, keyed[j].key) < 0
	})
	saved.Epochs = make([]epoch, len(keyed))
	for i := range keyed {
		saved.Epochs[i] = keyed[i].epoch
	}
	body, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	data, err := json.Marshal(envelope{SHA256: hex.EncodeToString(hash[:]), State: body})
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, errors.New("store exceeds size limit")
	}
	prepared := &preparedState{replica: r, checkpoint: cp, epochs: epochs, data: data}
	if s.metrics != nil {
		prepared.preparation = time.Since(started)
	}
	return prepared, nil
}

// commitPrepared does not read the clock: a known denial must retain the staged
// observation floor even if a later callback observes a clock regression.
func (s *Store) commitPrepared(prepared *preparedState) (resultErr error) {
	var started time.Time
	if s.metrics != nil {
		started = time.Now()
	}
	// Count only actual writer calls. Their elapsed time includes checkpointing
	// and serialization above; preflight refusals are not durable attempts.
	if s.metrics != nil {
		s.metrics.Add(runtimeobs.WorldStoreSerializedBytes, uint64(len(prepared.data)))
		defer func() {
			s.metrics.Observe(runtimeobs.WorldStoreCommit, prepared.preparation+time.Since(started))
			if resultErr != nil {
				s.metrics.Increment(runtimeobs.WorldStoreCommitFailures)
			}
		}()
	}
	if err := s.write(s.path, prepared.data); err != nil {
		s.fault = err
		return err
	}
	s.replica = prepared.replica
	s.epochs = prepared.epochs
	return nil
}

func (s *Store) persist(r *world.Replica, epochs map[world.Source]uint64) error {
	prepared, err := s.prepare(r, epochs)
	if err != nil {
		return err
	}
	return s.commitPrepared(prepared)
}

// PublicationState is an isolated copy of every retained register (including
// expired entries and tombstones), plus this publisher's locally allocated epoch.
type PublicationState struct {
	Epoch   uint64
	Records []world.Component
}

// ApplyPrepared groups publication inspection and application into one durable
// world write. prepare must synchronously validate policy and persist a proof
// superset covering both the old world and the candidate before returning nil.
// It runs under the Store mutex: it must not reenter Store, Node or authority, or
// retain aliases for concurrent use. The caller must enforce its own lock order.
// Known denials persist the captured observation-only checkpoint; a failed writer
// poisons the store and is never retried. No external effect may depend on success
// until this method returns nil. Existing Apply remains a separate primitive.
func (s *Store) ApplyPrepared(source world.Source, component world.Component, prepare func(PublicationState) error) (world.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prepare == nil {
		return "", errors.New("publication preparation callback required")
	}
	if err := source.Validate(); err != nil {
		return "", err
	}
	observation, err := s.candidate()
	if err != nil {
		return "", err
	}
	records, err := observation.Records()
	if err != nil {
		return "", err
	}
	staged, err := s.prepare(observation, s.epochs)
	if err != nil {
		return "", err
	}
	deny := func(reason error) (world.Result, error) {
		if err := s.commitPrepared(staged); err != nil {
			return "", err
		}
		return "", reason
	}
	// Copy the caller's component before invoking external preparation code.
	encoded, err := json.Marshal(component)
	if err != nil {
		return deny(err)
	}
	var copied world.Component
	if err = json.Unmarshal(encoded, &copied); err != nil {
		return deny(err)
	}
	candidate, err := observation.Clone()
	if err != nil {
		return deny(err)
	}
	if !s.subscription.Allows(copied.Key) {
		return deny(subscription.ErrExcluded)
	}
	if err = prepare(PublicationState{Epoch: s.epochs[source], Records: records}); err != nil {
		return deny(err)
	}
	result, err := candidate.Apply(source, copied)
	if err != nil {
		return deny(err)
	}
	if !s.subscription.Retains(copied) {
		if err := candidate.Fence(copied.Key, copied.Version); err != nil {
			return deny(err)
		}
	}
	publication, err := s.prepare(candidate, s.epochs)
	if err != nil {
		return deny(err)
	}
	if err = s.commitPrepared(publication); err != nil {
		return "", err
	}
	return result, nil
}

func (s *Store) Apply(source world.Source, c world.Component) (world.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return "", err
	}
	if !s.subscription.Allows(c.Key) {
		return "", subscription.ErrExcluded
	}
	result, err := r.Apply(source, c)
	if err != nil {
		return "", err
	}
	if !s.subscription.Retains(c) {
		if err := r.Fence(c.Key, c.Version); err != nil {
			return "", err
		}
	}
	if err := s.persist(r, s.epochs); err != nil {
		return "", err
	}
	return result, nil
}
func (s *Store) Snapshot() ([]world.Component, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return nil, err
	}
	result, err := r.Snapshot()
	if err != nil {
		return nil, err
	}
	if err := s.persist(r, s.epochs); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) Records() ([]world.Component, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return nil, err
	}
	result, err := r.Records()
	if err != nil {
		return nil, err
	}
	if err := s.persist(r, s.epochs); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) Get(k world.Key) (world.Component, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return world.Component{}, false, err
	}
	result, ok, err := r.Get(k)
	if err != nil {
		return world.Component{}, false, err
	}
	if err := s.persist(r, s.epochs); err != nil {
		return world.Component{}, false, err
	}
	return result, ok, nil
}
func (s *Store) SetFrame(f world.FrameRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return err
	}
	if err := r.SetFrame(f); err != nil {
		return err
	}
	return s.persist(r, s.epochs)
}

// MatchesFrame is a read-only lookup. Callers that also activate frames must
// serialize that activation with any admission policy depending on this result.
func (s *Store) MatchesFrame(frame world.FrameRef) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(); err != nil {
		return false, err
	}
	return s.replica.MatchesFrame(frame), nil
}

// CurrentEpoch returns the latest locally allocated epoch, or zero if this
// source has never requested one. Peer observations do not allocate epochs.
func (s *Store) CurrentEpoch(source world.Source) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := source.Validate(); err != nil {
		return 0, err
	}
	r, err := s.candidate()
	if err != nil {
		return 0, err
	}
	value := s.epochs[source]
	if err := s.persist(r, s.epochs); err != nil {
		return 0, err
	}
	return value, nil
}

func (s *Store) NextEpoch(source world.Source) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := source.Validate(); err != nil {
		return 0, err
	}
	r, err := s.candidate()
	if err != nil {
		return 0, err
	}
	if _, ok := s.epochs[source]; !ok && len(s.epochs) >= s.config.MaxEntries {
		return 0, world.ErrCapacity
	}
	last := s.epochs[source]
	all, err := r.Records()
	if err != nil {
		return 0, err
	}
	for _, c := range all {
		if c.Key.Source == source && c.Version.Epoch > last {
			last = c.Version.Epoch
		}
	}
	cp, err := r.Checkpoint()
	if err != nil {
		return 0, err
	}
	for _, f := range cp.Fences {
		if f.Key.Source == source && f.Version.Epoch > last {
			last = f.Version.Epoch
		}
	}
	if last == math.MaxUint64 {
		return 0, errors.New("publisher epoch exhausted")
	}
	updated := make(map[world.Source]uint64, len(s.epochs)+1)
	for k, v := range s.epochs {
		updated[k] = v
	}
	updated[source] = last + 1
	if err := s.persist(r, updated); err != nil {
		return 0, err
	}
	return last + 1, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lock == nil {
		return nil
	}
	err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	closeErr := s.lock.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".world-state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Subscription returns a detached immutable-policy description.
func (s *Store) Subscription() *subscription.Policy { return s.subscription.Clone() }
func (s *Store) AcceptsKey(key world.Key) bool      { return s.subscription.Allows(key) }

func (s *Store) Retains(c world.Component) bool { return s.subscription.Retains(c) }
