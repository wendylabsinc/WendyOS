// Package cloudlink delegates a hosted MCP connection to one Wendy Cloud login.
// It never reads the gateway operator's CLI credentials.
package cloudlink

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

type account struct {
	Client   string   `json:"client"`
	Scopes   []string `json:"scopes"`
	Expires  int64    `json:"expires"`
	Upstream []byte   `json:"upstream"`
}
type credential struct {
	Account string `json:"account"`
	Client  string `json:"client"`
	Expires int64  `json:"expires"`
	Used    bool   `json:"used,omitempty"`
}
type database struct {
	Version  int                   `json:"version"`
	Accounts map[string]account    `json:"accounts"`
	Access   map[string]credential `json:"access"`
	Refresh  map[string]credential `json:"refresh"`
}

type encryptedStore struct {
	path string
	aead cipher.AEAD
	aad  []byte
}

func openStore(path, encodedKey string, aad []byte) (*encryptedStore, database, error) {
	data := database{Version: 1, Accounts: map[string]account{}, Access: map[string]credential{}, Refresh: map[string]credential{}}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, data, errors.New("Cloud link encryption key must be base64 containing exactly 32 bytes")
	}
	if !filepath.IsAbs(path) {
		return nil, data, errors.New("Cloud link state_file must be an absolute path")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	s := &encryptedStore{path, aead, aad}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, data, nil
	}
	if err != nil {
		return nil, data, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32<<20 {
		return nil, data, errors.New("Cloud link state must be a private regular file no larger than 32 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, data, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, data, errors.New("invalid encrypted Cloud link state")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], aad)
	if err != nil {
		return nil, data, errors.New("Cloud link state cannot be decrypted with this key and service configuration")
	}
	if json.Unmarshal(plain, &data) != nil || data.Version != 1 || data.Accounts == nil || data.Access == nil || data.Refresh == nil || len(data.Accounts) > 10000 || len(data.Access) > 20000 || len(data.Refresh) > 20000 {
		return nil, data, errors.New("invalid Cloud link database")
	}
	return s, data, nil
}

func (s *encryptedStore) save(data database) error {
	plain, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(plain) > 32<<20 {
		return errors.New("Cloud link state exceeds 32 MiB")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	raw := s.aead.Seal(nonce, nonce, plain, s.aad)
	// Do not create directories under a possibly public or operator-owned path.
	// Deployment must provision a private directory and one writer process.
	return atomicfile.Write(s.path, raw, 0600)
}

func clone(data database) database {
	raw, _ := json.Marshal(data)
	var copied database
	_ = json.Unmarshal(raw, &copied)
	return copied
}

func (d *database) prune(now time.Time) {
	for id, a := range d.Accounts {
		if a.Expires <= now.Unix() {
			delete(d.Accounts, id)
		}
	}
	for hash, token := range d.Access {
		if _, ok := d.Accounts[token.Account]; !ok || token.Expires <= now.Unix() {
			delete(d.Access, hash)
		}
	}
	for hash, token := range d.Refresh {
		if _, ok := d.Accounts[token.Account]; !ok || token.Expires <= now.Unix() {
			delete(d.Refresh, hash)
		}
	}
}
func secret() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func tokenHash(raw string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) }

// accountStore saves only the linked account selected by its unguessable ID.
type accountStore struct {
	owner *Manager
	id    string
}

func (s accountStore) Load() ([]byte, error) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	if s.owner.closed {
		return nil, errors.New("Cloud link service closed")
	}
	a, ok := s.owner.data.Accounts[s.id]
	if !ok || a.Expires <= time.Now().Unix() {
		return nil, errors.New("Cloud connection expired or revoked")
	}
	return append([]byte(nil), a.Upstream...), nil
}
func (s accountStore) Save(raw []byte) error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	a, ok := s.owner.data.Accounts[s.id]
	if !ok || a.Expires <= time.Now().Unix() {
		return errors.New("Cloud connection expired or revoked")
	}
	if len(raw) > 1<<20 {
		return errors.New("Cloud credential record too large")
	}
	a.Upstream = append([]byte(nil), raw...)
	next := clone(s.owner.data)
	next.Accounts[s.id] = a
	return s.owner.commitLocked(next)
}
func (s accountStore) Clear() error { return s.owner.revokeAccount(s.id) }

type memoryStore struct{ raw []byte }

func (s *memoryStore) Load() ([]byte, error) { return append([]byte(nil), s.raw...), nil }
func (s *memoryStore) Save(raw []byte) error { s.raw = append([]byte(nil), raw...); return nil }
func (s *memoryStore) Clear() error          { s.raw = nil; return nil }
