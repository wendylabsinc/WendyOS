// Package trustedkeys holds the access setting of a device that is not
// enrolled: Open (no file) or Closed (the file exists). A Closed device lists
// plain public keys, each with exactly one of three roles, and only callers
// proving one of those keys get in. There is no certificate authority: a
// caller proves its key with a self-signed client certificate.
//
// The file is kept when the device enrolls (it is not used while enrolled)
// and is in force again after unenrolling, so unenrolling a Closed device
// returns it to Closed with its keys.
package trustedkeys

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// FileName is the access file inside the agent's config directory.
const FileName = "trusted-keys.json"

// AddedByInstall is the added_by value of a key seeded by the installer.
const AddedByInstall = "install"

// shapeClosed is the only shape the file stores: no file means Open.
const shapeClosed = "closed"

// Role is ordered: a higher role includes every right of the lower ones.
type Role int

const (
	RoleNone Role = iota
	RoleViewer
	RoleOperator
	RoleOwner
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleOwner:
		return "owner"
	default:
		return "none"
	}
}

// ParseRole accepts the three role names; anything else is an error.
func ParseRole(s string) (Role, error) {
	switch s {
	case "viewer":
		return RoleViewer, nil
	case "operator":
		return RoleOperator, nil
	case "owner":
		return RoleOwner, nil
	}
	return RoleNone, fmt.Errorf("unknown role %q (want owner, operator or viewer)", s)
}

var (
	ErrAlreadyClosed = errors.New("this device is already closed; ask its owner to add your key")
	ErrNotClosed     = errors.New("this device is open; close it first with your key")
	ErrNotFound      = errors.New("no trusted key has that fingerprint; list the keys to see them")
	ErrAlreadyExists = errors.New("that key is already trusted; remove it first to change its role")
	ErrLastOwner     = errors.New("that is the last owner key; add another owner first")
	ErrActorNotOwner = errors.New("your key is no longer an owner on this device")
)

// Entry is one trusted key as stored on disk.
type Entry struct {
	// PublicKey is the SubjectPublicKeyInfo as PEM ("PUBLIC KEY").
	PublicKey string    `json:"public_key"`
	Role      string    `json:"role"`
	Label     string    `json:"label,omitempty"`
	AddedAt   time.Time `json:"added_at"`
	// AddedBy is the fingerprint of the key that added this one, "install"
	// for a seeded key, or empty for the key that closed the device.
	AddedBy string `json:"added_by,omitempty"`

	spki []byte
	role Role
	fp   string
}

// SPKI returns the SubjectPublicKeyInfo DER.
func (e Entry) SPKI() []byte { return e.spki }

// Fingerprint is "sha256:" + hex SHA-256 of the SPKI DER.
func (e Entry) Fingerprint() string { return e.fp }

// KeyRole is the parsed role.
func (e Entry) KeyRole() Role { return e.role }

type fileFormat struct {
	Shape       string  `json:"shape"`
	KeylessApps bool    `json:"keyless_apps"`
	Keys        []Entry `json:"keys"`
}

// Store is the loaded file. It is safe for concurrent use; the agent is the
// only writer while it runs.
type Store struct {
	path string
	mu   sync.RWMutex
	file *fileFormat // nil: Open
}

// Fingerprint returns "sha256:<hex>" of a SubjectPublicKeyInfo DER.
func Fingerprint(spki []byte) string {
	sum := sha256.Sum256(spki)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParsePublicKey accepts a SubjectPublicKeyInfo as PEM ("PUBLIC KEY") or DER
// and returns the DER after checking it parses.
func ParsePublicKey(b []byte) ([]byte, error) {
	der := b
	if block, _ := pem.Decode(b); block != nil {
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("the key is a %q PEM block; send a PUBLIC KEY", block.Type)
		}
		der = block.Bytes
	}
	if _, err := x509.ParsePKIXPublicKey(der); err != nil {
		return nil, fmt.Errorf("the public key could not be read (%v); send a PEM or DER SubjectPublicKeyInfo", err)
	}
	return der, nil
}

// Open loads path. A missing file is an Open device. A file that exists but
// cannot be read or validated is an error: the caller must fail closed, never
// fall back to an Open device.
func Open(path string) (*Store, error) {
	f, err := load(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, file: f}, nil
}

func load(path string) (*fileFormat, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Whoever can write this file can make themselves owner.
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by group or others (mode %#o); fix with chmod 600", path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func parse(data []byte) (*fileFormat, error) {
	var f fileFormat
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing trusted keys: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parsing trusted keys: trailing data after the JSON object")
	}
	if f.Shape != shapeClosed {
		return nil, fmt.Errorf("shape is %q; the file only exists for a closed device", f.Shape)
	}
	seen := make(map[string]bool, len(f.Keys))
	owners := 0
	for i := range f.Keys {
		e := &f.Keys[i]
		spki, err := ParsePublicKey([]byte(e.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if e.role, err = ParseRole(e.Role); err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		e.spki, e.fp = spki, Fingerprint(spki)
		if seen[e.fp] {
			return nil, fmt.Errorf("key %d: duplicate key %s", i, e.fp)
		}
		seen[e.fp] = true
		if e.role == RoleOwner {
			owners++
		}
	}
	if owners == 0 {
		return nil, errors.New("a closed device needs at least one owner key")
	}
	return &f, nil
}

// Closed reports whether the device is Closed (the file exists).
func (s *Store) Closed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file != nil
}

// KeylessApps reports whether the owner opened the fixed "deploy and manage
// apps" set to callers without a key.
func (s *Store) KeylessApps() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file != nil && s.file.KeylessApps
}

// Lookup returns the role of the key with this SPKI DER (RoleNone when it is
// not trusted or the device is Open) and its fingerprint.
func (s *Store) Lookup(spki []byte) (Role, string) {
	fp := Fingerprint(spki)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file != nil {
		for _, e := range s.file.Keys {
			if e.fp == fp {
				return e.role, fp
			}
		}
	}
	return RoleNone, fp
}

// List returns a copy of the trusted keys.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return nil
	}
	return append([]Entry(nil), s.file.Keys...)
}

// Floor is the latest added_at: a lower bound for the device clock when
// checking a client certificate's NotBefore on a device without an RTC.
func (s *Store) Floor() time.Time {
	var floor time.Time
	for _, e := range s.List() {
		if e.AddedAt.After(floor) {
			floor = e.AddedAt
		}
	}
	return floor
}

func newEntry(spki []byte, role Role, label, addedBy string, now time.Time) (Entry, error) {
	if role == RoleNone {
		return Entry{}, errors.New("a role is required: owner, operator or viewer")
	}
	if _, err := x509.ParsePKIXPublicKey(spki); err != nil {
		return Entry{}, fmt.Errorf("the public key could not be read (%v); send a PEM or DER SubjectPublicKeyInfo", err)
	}
	return Entry{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})),
		Role:      role.String(),
		Label:     label,
		AddedAt:   now.UTC().Truncate(time.Second),
		AddedBy:   addedBy,
		spki:      spki,
		role:      role,
		fp:        Fingerprint(spki),
	}, nil
}

// Close turns an Open device Closed with spki as its first owner. Anyone may
// close an Open device.
func (s *Store) Close(spki []byte, label string, now time.Time) (Entry, error) {
	owner, err := newEntry(spki, RoleOwner, label, "", now)
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return Entry{}, ErrAlreadyClosed
	}
	return owner, s.write(&fileFormat{Shape: shapeClosed, Keys: []Entry{owner}})
}

// Seed returns the access file the installer writes to the config partition:
// a Closed device with spki as its only (owner) key, keyless apps off.
func Seed(spki []byte, label string, now time.Time) ([]byte, error) {
	owner, err := newEntry(spki, RoleOwner, label, AddedByInstall, now)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(&fileFormat{Shape: shapeClosed, Keys: []Entry{owner}}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Reopen turns a Closed device Open, dropping every key. actor must be an
// owner when the write happens.
func (s *Store) Reopen(actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOwner(actor); err != nil {
		return err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", s.path, err)
	}
	s.file = nil
	return nil
}

// SetKeylessApps opens or shuts the keyless "deploy and manage apps" set.
func (s *Store) SetKeylessApps(enabled bool, actor string) error {
	return s.mutate(actor, func(f *fileFormat) error {
		f.KeylessApps = enabled
		return nil
	})
}

// Add trusts a new key with one role.
func (s *Store) Add(spki []byte, role Role, label, actor string, now time.Time) (Entry, error) {
	added, err := newEntry(spki, role, label, actor, now)
	if err != nil {
		return Entry{}, err
	}
	return added, s.mutate(actor, func(f *fileFormat) error {
		for _, e := range f.Keys {
			if e.fp == added.fp {
				return ErrAlreadyExists
			}
		}
		f.Keys = append(f.Keys, added)
		return nil
	})
}

// Remove drops the key with this fingerprint. The last owner key cannot be
// removed, so a Closed device always has an owner.
func (s *Store) Remove(fingerprint, actor string) (Entry, error) {
	var removed Entry
	err := s.mutate(actor, func(f *fileFormat) error {
		idx, owners := -1, 0
		for i, e := range f.Keys {
			if e.role == RoleOwner {
				owners++
			}
			if e.fp == fingerprint {
				idx = i
			}
		}
		if idx < 0 {
			return ErrNotFound
		}
		if f.Keys[idx].role == RoleOwner && owners == 1 {
			return ErrLastOwner
		}
		removed = f.Keys[idx]
		f.Keys = append(f.Keys[:idx:idx], f.Keys[idx+1:]...)
		return nil
	})
	return removed, err
}

// checkOwner runs under the write lock: the authorizer's role check ran
// before it, and an owner removed in between must not still write.
func (s *Store) checkOwner(actor string) error {
	if s.file == nil {
		return ErrNotClosed
	}
	for _, e := range s.file.Keys {
		if e.fp == actor && e.role == RoleOwner {
			return nil
		}
	}
	return ErrActorNotOwner
}

func (s *Store) mutate(actor string, fn func(*fileFormat) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOwner(actor); err != nil {
		return err
	}
	next := *s.file
	next.Keys = append([]Entry(nil), s.file.Keys...)
	if err := fn(&next); err != nil {
		return err
	}
	return s.write(&next)
}

func (s *Store) write(f *fileFormat) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing trusted keys: %w", err)
	}
	s.file = f
	return nil
}

type keylessKey struct{}

// WithKeyless marks ctx as a call admitted without a key (the keyless apps
// set), so the deploy path can refuse what that set must not grant.
func WithKeyless(ctx context.Context) context.Context {
	return context.WithValue(ctx, keylessKey{}, true)
}

// Keyless reports whether ctx is a call admitted without a key.
func Keyless(ctx context.Context) bool {
	v, _ := ctx.Value(keylessKey{}).(bool)
	return v
}
