package authority

import (
	"context"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ReplayStore atomically records nonces, revocation high-water marks and trusted
// clock observations. Durable implementations must commit before returning nil.
type ReplayStore interface {
	Consume(key string, expires, now time.Time) error
	Checkpoint(revision uint64, digest string, now time.Time) error
	ValidateCheckpoint(revision uint64, digest string, now time.Time) error
}

// ReplayAdmissionStore optionally combines an authenticated request's checkpoint
// and nonce consumption in one durable admission. Implementations must retain
// checkpoint advancement on nonce denial, just as Checkpoint followed by Consume
// does. Callers must authenticate the request before invoking this method.
type ReplayAdmissionStore interface {
	ReplayStore
	CheckpointAndConsume(revision uint64, digest, key string, expires, now time.Time) error
}

var errReplayStateTooLarge = errors.New("replay state exceeds size limit")

type replayState struct {
	Version    int              `json:"version"`
	Nonces     map[string]int64 `json:"nonces"`
	Revision   uint64           `json:"revision"`
	Digest     string           `json:"digest"`
	LastUnixMS int64            `json:"last_unix_ms"`
}
type ReplayCache struct {
	metrics         *runtimeobs.Collector
	beforeCommit    func()
	syncDirectory   func(string) error        // optional private test seam after rename
	appendWrite     func([]byte) (int, error) // test-only journal write seam
	syncFile        func(*os.File) error      // test-only file sync seam
	file            *os.File
	sequence        uint64
	offset          int64
	appendedBytes   int64
	appendedEntries uint64
	previous        [32]byte
	nonceBytes      int64
	mu              sync.Mutex
	state           replayState
	max             int
	path            string
	staging         string
	lock            *os.File
	closed          bool
	poisoned        bool
}

func NewMemoryReplay(maxEntries int) *ReplayCache {
	return &ReplayCache{state: replayState{Version: Version, Nonces: map[string]int64{}}, max: maxEntries}
}

// OpenReplay opens a private replay journal, migrating legacy snapshots under its lock. The
// directory must be trusted local storage. External disk rollback is not detected.
func OpenReplay(path string, maxEntries int) (*ReplayCache, error) {
	if path == "" || maxEntries <= 0 {
		return nil, errors.New("invalid replay store configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(abs))
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if info, statErr := lock.Stat(); statErr != nil || !info.Mode().IsRegular() {
		_ = lock.Close()
		return nil, errors.New("replay lock must be a regular file")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock replay store: %w", err)
	}
	c := NewMemoryReplay(maxEntries)
	c.path = path
	c.lock = lock
	if err = c.openJournal(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
func (c *ReplayCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var fileErr error
	if c.file != nil {
		fileErr = c.file.Close()
		c.file = nil
	}
	if c.lock != nil {
		err := syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
		closeErr := c.lock.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return fileErr
}
func (c *ReplayCache) candidate(now time.Time) (replayState, error) {
	if c.closed || c.poisoned || c.max <= 0 {
		return replayState{}, errors.New("replay store unavailable")
	}
	if now.UnixMilli() <= 0 || now.UnixMilli() < c.state.LastUnixMS {
		return replayState{}, errors.New("authority clock regressed")
	}
	s := c.state
	s.LastUnixMS = now.UnixMilli()
	s.Nonces = make(map[string]int64, len(c.state.Nonces))
	for k, expiry := range c.state.Nonces {
		if expiry > now.UnixMilli() {
			s.Nonces[k] = expiry
		}
	}
	return s, nil
}
func (c *ReplayCache) Consume(key string, expires, now time.Time) error {
	return c.ConsumeContext(context.Background(), key, expires, now)
}
func (c *ReplayCache) ConsumeContext(ctx context.Context, key string, expires, now time.Time) error {
	wait := diagnostics.Begin(ctx, diagnostics.ReplayLock)
	c.mu.Lock()
	wait.End()
	defer c.mu.Unlock()
	s, err := c.candidate(now)
	if err != nil {
		return err
	}
	if key == "" || len(key) > 1024 || !expires.After(now) {
		return ErrDenied
	}
	if _, ok := s.Nonces[key]; ok {
		return ErrReplay
	}
	if len(s.Nonces) >= c.max {
		return errors.New("replay store capacity reached")
	}
	s.Nonces[key] = expires.UnixMilli()
	return c.commitContext(ctx, s)
}

// CheckpointAndConsume commits a valid checkpoint and new nonce together. A
// nonce refusal still persists a changed checkpoint before returning its error;
// failure to persist that checkpoint takes precedence over the nonce refusal.
// A failed durable write never admits a request or triggers a second write.
// Unlike two separate transactions, a pre-rename I/O failure may leave both the
// nonce and checkpoint unchanged. No authority is returned on that failure.
func (c *ReplayCache) CheckpointAndConsume(revision uint64, digest, key string, expires, now time.Time) error {
	return c.CheckpointAndConsumeContext(context.Background(), revision, digest, key, expires, now)
}
func (c *ReplayCache) CheckpointAndConsumeContext(ctx context.Context, revision uint64, digest, key string, expires, now time.Time) error {
	wait := diagnostics.Begin(ctx, diagnostics.ReplayLock)
	c.mu.Lock()
	wait.End()
	defer c.mu.Unlock()
	s, err := c.candidate(now)
	if err != nil {
		return err
	}
	if revision == 0 || digest == "" || revision < s.Revision || (revision == s.Revision && digest != s.Digest) {
		return errors.New("revocation revision rollback or conflict")
	}
	s.Revision, s.Digest = revision, digest
	checkpointChanged := s.LastUnixMS != c.state.LastUnixMS || s.Revision != c.state.Revision || s.Digest != c.state.Digest
	deny := func(reason error) error {
		if checkpointChanged {
			if err := c.commitContext(ctx, s); err != nil {
				return err
			}
		} else {
			c.metrics.Increment(runtimeobs.ReplayCheckpointNoops)
		}
		return reason
	}
	if key == "" || len(key) > 1024 || !expires.After(now) {
		return deny(ErrDenied)
	}
	if _, ok := s.Nonces[key]; ok {
		return deny(ErrReplay)
	}
	if len(s.Nonces) >= c.max {
		return deny(errors.New("replay store capacity reached"))
	}
	s.Nonces[key] = expires.UnixMilli()
	err = c.commitContext(ctx, s)
	if errors.Is(err, errReplayStateTooLarge) {
		// Serialization bounds are checked before any write. Preserve the
		// checkpoint that a separate Checkpoint call would have committed.
		delete(s.Nonces, key)
		return deny(err)
	}
	return err
}

// ValidateCheckpoint is read-only and does not reserve or advance a revision.
func (c *ReplayCache) ValidateCheckpoint(revision uint64, digest string, now time.Time) error {
	return c.ValidateCheckpointContext(context.Background(), revision, digest, now)
}
func (c *ReplayCache) ValidateCheckpointContext(ctx context.Context, revision uint64, digest string, now time.Time) error {
	wait := diagnostics.Begin(ctx, diagnostics.ReplayLock)
	c.mu.Lock()
	wait.End()
	defer c.mu.Unlock()
	s, err := c.candidate(now)
	if err != nil {
		return err
	}
	if revision == 0 || digest == "" || revision < s.Revision || (revision == s.Revision && digest != s.Digest) {
		return errors.New("revocation revision rollback or conflict")
	}
	return nil
}
func (c *ReplayCache) Checkpoint(revision uint64, digest string, now time.Time) error {
	return c.CheckpointContext(context.Background(), revision, digest, now)
}
func (c *ReplayCache) CheckpointContext(ctx context.Context, revision uint64, digest string, now time.Time) error {
	wait := diagnostics.Begin(ctx, diagnostics.ReplayLock)
	c.mu.Lock()
	wait.End()
	defer c.mu.Unlock()
	s, err := c.candidate(now)
	if err != nil {
		return err
	}
	if revision == 0 || digest == "" || revision < s.Revision || (revision == s.Revision && digest != s.Digest) {
		return errors.New("revocation revision rollback or conflict")
	}
	s.Revision = revision
	s.Digest = digest
	if s.LastUnixMS == c.state.LastUnixMS && s.Revision == c.state.Revision && s.Digest == c.state.Digest {
		c.metrics.Increment(runtimeobs.ReplayCheckpointNoops)
		return nil
	}
	return c.commitContext(ctx, s)
}
func (c *ReplayCache) commit(s replayState) (resultErr error) {
	return c.commitContext(context.Background(), s)
}
func (c *ReplayCache) commitContext(ctx context.Context, s replayState) (resultErr error) {
	if c.path == "" {
		c.state = s
		return nil
	}
	phase := diagnostics.Begin(ctx, diagnostics.ReplayCommit)
	defer func() { phase.End(resultErr) }()
	var started time.Time
	if c.metrics != nil {
		started = time.Now()
	}
	mutation, nonceBytes, err := c.prepareMutation(s)
	if err != nil {
		return err
	}
	data, err := encodeJournalFrame(mutation)
	if err != nil {
		return err
	}
	if c.metrics != nil {
		defer func() {
			c.metrics.Observe(runtimeobs.ReplayCheckpointCommit, time.Since(started))
			if resultErr != nil {
				c.metrics.Increment(runtimeobs.ReplayCheckpointFailures)
			}
		}()
	}
	if c.beforeCommit != nil {
		c.beforeCommit()
	}
	if c.file == nil || c.appendedEntries >= journalCompactEntries || c.appendedBytes+int64(len(data)) > journalCompactBytes {
		replace := diagnostics.Begin(ctx, diagnostics.ReplayReplace)
		err = c.replaceJournal(s, c.sequence+1)
		replace.End(err)
		if err != nil {
			return err
		}
	} else {
		appendPhase := diagnostics.Begin(ctx, diagnostics.ReplayAppend)
		n := 0
		if c.appendWrite != nil {
			n, err = c.appendWrite(data)
		} else {
			n, err = c.file.WriteAt(data, c.offset)
		}
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		appendPhase.End(err)
		if err == nil {
			syncPhase := diagnostics.Begin(ctx, diagnostics.ReplaySync)
			err = c.sync(c.file)
			syncPhase.End(err)
		}
		if err != nil {
			c.poisoned = true
			return fmt.Errorf("uncertain replay append: %w", err)
		}
		c.offset += int64(len(data))
		c.appendedBytes += int64(len(data))
		c.appendedEntries++
		c.sequence++
		c.previous = journalPayloadHash(data)
	}
	c.state = s
	c.nonceBytes = nonceBytes
	return nil
}
