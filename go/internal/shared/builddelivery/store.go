package builddelivery

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const MetadataKey = "wendy-build-delivery"
const HTTPHeader = "Wendy-Build-Delivery"
const MaxActiveSessions = 1024

type record struct {
	Contract   string    `json:"contract"`
	AdmittedAt time.Time `json:"admitted_at"`
	// Output is durable before publication. A crash may leave pending output;
	// retry can finish only this exact digest, never choose a different one.
	Output    string `json:"output,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Completed bool   `json:"completed,omitempty"`
}
type session struct {
	verified Verified
	record   record
}
type Store struct {
	mu       sync.Mutex
	closed   bool
	release  func()
	dir      string
	target   meshid.Device
	roots    []*x509.Certificate
	now      func() time.Time
	sessions map[string]*session
	jobs     map[string]string
}

// Open reloads only locally admitted, signature-verified records. An invalid
// record is a startup error; silently losing replay/CAS state would reopen a
// completed authorization. dir must be on the device's persistent data volume.
func Open(dir string, roots []*x509.Certificate, target meshid.Device, clock func() time.Time) (*Store, error) {
	if dir == "" || len(roots) == 0 || !target.IsValid() || clock == nil {
		return nil, errors.New("delivery store requires persistent path, local roots and target")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	release, err := flock.Acquire(filepath.Join(dir, ".lock"), 0)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			release()
		}
	}()
	now := clock()
	s := &Store{release: release, now: clock, dir: dir, target: target, roots: append([]*x509.Certificate(nil), roots...), sessions: map[string]*session{}, jobs: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Size() > MaxContractBytes+4096 {
			return nil, errors.New("oversized persisted delivery session")
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var r record
		if err = strictJSON(data, &r); err != nil {
			return nil, err
		}
		v, err := Admit(r.Contract, s.roots, target, r.AdmittedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid persisted delivery admission: %w", err)
		}
		id := sessionID(r.Contract)
		if entry.Name() != id+".json" || r.AdmittedAt.After(now) {
			return nil, errors.New("invalid delivery record name or future admission")
		}
		if now.Unix() >= v.claims.ValidUntil {
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if len(s.sessions) >= MaxActiveSessions {
			return nil, errors.New("too many persisted delivery sessions")
		}
		if _, exists := s.jobs[v.claims.JobID]; exists {
			return nil, errors.New("duplicate persisted delivery job")
		}
		s.sessions[id] = &session{verified: v, record: r}
		s.jobs[v.claims.JobID] = id
	}
	opened = true
	return s, nil
}
func sessionID(compact string) string { hash := sha256.Sum256([]byte(compact)); return encode(hash[:]) }
func (s *Store) persist(id string, r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.dir, id+".json"), data, 0600)
}

func (s *Store) Admit(compact string, peer *x509.Certificate) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("delivery store closed")
	}
	if peer == nil {
		return "", errors.New("authenticated admission caller required")
	}
	now := s.now()
	id := sessionID(compact)
	if old, ok := s.sessions[id]; ok {
		if err := checkBuilder(old.verified.claims, peer); err != nil {
			identity, found, e := certs.IdentityFromCert(peer)
			if e != nil || !found || identity.Principal != old.verified.claims.Operator || identity.EntityType != certs.EntityUser {
				return "", err
			}
		}
		if now.Unix() >= old.verified.claims.ValidUntil {
			return "", errors.New("delivery session expired")
		}
		return id, nil
	}
	v, err := Admit(compact, s.roots, s.target, now)
	if err != nil {
		return "", err
	}

	identity, found, e := certs.IdentityFromCert(peer)
	if e != nil || !found {
		return "", errors.New("invalid admission caller")
	}
	if identity.EntityType != certs.EntityUser || identity.Principal != v.claims.Operator {
		if err = checkBuilder(v.claims, peer); err != nil {
			return "", err
		}
	}
	for oldID, entry := range s.sessions {
		if now.Unix() >= entry.verified.claims.ValidUntil {
			if err = os.Remove(filepath.Join(s.dir, oldID+".json")); err != nil {
				return "", err
			}
			delete(s.jobs, entry.verified.claims.JobID)
			delete(s.sessions, oldID)
		}
	}
	if _, exists := s.jobs[v.claims.JobID]; exists {
		return "", errors.New("delivery job already has a different authorization")
	}
	if len(s.sessions) >= MaxActiveSessions {
		return "", errors.New("delivery session capacity reached")
	}
	r := record{Contract: compact, AdmittedAt: now}
	if err = s.persist(id, r); err != nil {
		return "", err
	}
	s.sessions[id] = &session{verified: v, record: r}
	s.jobs[v.claims.JobID] = id
	return id, nil
}
func checkBuilder(c Claims, peer *x509.Certificate) error {
	if peer == nil {
		return errors.New("authenticated builder required")
	}
	id, found, err := certs.IdentityFromCert(peer)
	if err != nil || !found || id.EntityType != certs.EntityAsset || id.Principal != c.Builder || Fingerprint(peer) != c.BuilderSPKI {
		return errors.New("delivery session requires the authorized builder key")
	}
	return nil
}
func (s *Store) authorize(id string, peer *x509.Certificate, now time.Time) (*session, error) {
	if s.closed {
		return nil, errors.New("delivery store closed")
	}
	b, err := decode(id)
	if err != nil || len(b) != 32 {
		return nil, errors.New("invalid delivery session")
	}
	entry, ok := s.sessions[id]
	if !ok {
		return nil, errors.New("unknown delivery session")
	}
	if err = checkBuilder(entry.verified.claims, peer); err != nil {
		return nil, err
	}
	if now.Unix() >= entry.verified.claims.ValidUntil || now.Before(entry.record.AdmittedAt) {
		return nil, errors.New("delivery session expired or clock regressed")
	}
	return entry, nil
}
func (s *Store) Authorize(id string, peer *x509.Certificate, reference string) (Claims, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.authorize(id, peer, s.now())
	if err != nil {
		return Claims{}, err
	}
	if reference != "" && reference != entry.verified.claims.Reference {
		return Claims{}, errors.New("delivery reference is outside the operator grant")
	}
	if entry.record.Completed {
		return Claims{}, errors.New("delivery already completed")
	}
	return entry.verified.Claims(), nil
}

// Publish serializes the CAS and durable output choice across both transports.
// apply must compare the current reference with expected before changing it;
// if it already equals output, it must succeed (crash/retry recovery). It must
// not re-enter this Store. Authorization is checked immediately before apply.
func (s *Store) Publish(id string, peer *x509.Certificate, reference, output string, apply func(expected, output string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.authorize(id, peer, s.now())
	if err != nil {
		return err
	}
	if reference != entry.verified.claims.Reference {
		return errors.New("delivery reference is outside the operator grant")
	}
	if len(output) != 71 || output[:7] != "sha256:" {
		return errors.New("invalid publication digest")
	}
	for _, ch := range output[7:] {
		if !stringsContainsHex(ch) {
			return errors.New("invalid publication digest")
		}
	}
	if entry.record.Output != "" && entry.record.Output != output {
		return errors.New("delivery job is already bound to another output")
	}
	if entry.record.Completed {
		return nil
	}
	if entry.record.Output == "" {
		r := entry.record
		r.Output = output
		if err = s.persist(id, r); err != nil {
			return err
		}
		entry.record = r
	}
	if _, err = s.authorize(id, peer, s.now()); err != nil {
		return err
	}
	if err = apply(entry.verified.claims.ExpectedDigest, output); err != nil {
		return err
	}
	r := entry.record
	r.Completed = true
	if err = s.persist(id, r); err != nil {
		return err
	}
	entry.record = r
	return nil
}
func stringsContainsHex(ch rune) bool { return ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' }

// ReserveBytes charges the durable session before accepting caller bytes. A
// reconnect cannot reset its resource budget. Cache hits and reads cost none.
func (s *Store) ReserveBytes(id string, peer *x509.Certificate, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.authorize(id, peer, s.now())
	if err != nil {
		return err
	}
	if n < 0 || entry.record.Completed || n > entry.verified.claims.MaxBytes-entry.record.Bytes {
		return errors.New("delivery byte budget exceeded or output fixed")
	}
	r := entry.record
	r.Bytes += n
	if err = s.persist(id, r); err != nil {
		return err
	}
	entry.record = r
	return nil
}

// Direct serializes an ambient operator/local-admin image mutation with delegated
// CAS publication. Every runtime and registry writer of the same image store
// must use this shared Store, including writes that do not carry a session.
func (s *Store) Direct(apply func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("delivery store closed")
	}
	return apply()
}
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.release()
	}
}
