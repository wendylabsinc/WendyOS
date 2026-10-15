package localmesh

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	MaxManifestBytes = 32768
	MaxLease         = 2 * time.Minute
	manifestDomain   = "wendy-local-mesh/manifest/1\x00"
)

type Manifest struct {
	Version    int `json:"version"`
	Org, Asset int32
	// Revision is a persistently increasing origin counter, including restart.
	Revision        uint64 `json:"revision"`
	Issued, Expires int64
	Name            string `json:"name"`
	AgentPort       uint16 `json:"agentPort"`
	Internet        bool   `json:"internet"`
	Withdraw        bool   `json:"withdraw,omitempty"`
}

type SignedManifest struct {
	Fingerprint string          `json:"fingerprint"`
	Body        json.RawMessage `json:"body"`
	Signature   []byte          `json:"signature"`
}

var ErrExpiredManifest = errors.New("expired local-mesh manifest")
var ErrStaleManifest = errors.New("stale local-mesh manifest")

func (m Manifest) validate(now time.Time) error {
	issued, expires := time.UnixMilli(m.Issued), time.UnixMilli(m.Expires)
	if !expires.After(now) {
		return ErrExpiredManifest
	}
	if m.Version != 1 || m.Org <= 0 || m.Asset <= 0 || m.Asset > 65534 || m.Revision == 0 ||
		issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > MaxLease ||
		len(m.Name) > 255 || (!m.Withdraw && m.AgentPort == 0) {
		return errors.New("invalid or expired local-mesh manifest")
	}
	if m.Withdraw && m.Internet {
		return errors.New("withdrawal advertises internet")
	}
	return nil
}

func signingInput(key crypto.PublicKey, body []byte) ([]byte, crypto.SignerOpts) {
	input := append([]byte(manifestDomain), body...)
	if _, ok := key.(ed25519.PublicKey); ok {
		return input, crypto.Hash(0)
	}
	h := sha256.Sum256(input)
	return h[:], crypto.SHA256
}

func SignManifest(m Manifest, chain [][]byte, signer crypto.Signer, now time.Time) (SignedManifest, error) {
	var out SignedManifest
	if err := m.validate(now); err != nil {
		return out, err
	}
	if signer == nil || len(chain) == 0 {
		return out, errors.New("missing manifest signer")
	}
	out.Fingerprint = Fingerprint(chain)
	body, err := json.Marshal(m)
	if err != nil {
		return out, err
	}
	if len(body) > MaxManifestBytes {
		return out, errors.New("oversized manifest")
	}
	input, opts := signingInput(signer.Public(), body)
	sig, err := signer.Sign(rand.Reader, input, opts)
	if err != nil {
		return out, err
	}
	out.Body, out.Signature = body, sig
	return out, nil
}

// ErrIdentityNeeded lets the synchronizer request an independently validated
// bundle from its neighbour. Unverified announcements are not retained here.
var ErrIdentityNeeded = errors.New("local-mesh identity bundle needed")

type directoryEntry struct {
	manifest Manifest
	wire     SignedManifest
	hash     string
	// Tombstones and superseded versions live for a full maximum lease from
	// admission, so removing a short-lived update cannot resurrect an older one.
	retainUntil time.Time
}

type Directory struct {
	mu      sync.Mutex
	org     int32
	limit   int
	cache   *IdentityCache
	entries map[int32]directoryEntry
	history map[int32]DirectoryReceipt
	persist func([]DirectoryReceipt) error
}

// DirectoryReceipt is durable replay protection, not cached presence.
type DirectoryReceipt struct {
	Asset    int32
	Revision uint64
	Hash     string
	Until    time.Time
}

func (d *Directory) SetPersistence(receipts []DirectoryReceipt, persist func([]DirectoryReceipt) error, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(receipts) > d.limit || persist == nil {
		return errors.New("invalid directory persistence")
	}
	history := map[int32]DirectoryReceipt{}
	for _, r := range receipts {
		if r.Asset <= 0 || r.Asset > 65534 || r.Revision == 0 || len(r.Hash) != 64 {
			return errors.New("invalid directory receipt")
		}
		if r.Until.After(now) {
			history[r.Asset] = r
		}
	}
	d.history = history
	d.persist = persist
	return nil
}

func NewDirectory(org int32, limit int, cache *IdentityCache) (*Directory, error) {
	if org <= 0 || limit < 1 || cache == nil {
		return nil, errors.New("invalid directory configuration")
	}
	return &Directory{org: org, limit: limit, cache: cache, entries: map[int32]directoryEntry{}}, nil
}

// Accept validates the origin, not the relaying neighbour. Return true only for
// a new version: duplicate delivery never renews its signed absolute expiry.
func (d *Directory) Accept(w SignedManifest, now time.Time) (bool, error) {
	if len(w.Body) > MaxManifestBytes || len(w.Signature) > 1024 || len(w.Fingerprint) != 64 {
		return false, errors.New("oversized manifest envelope")
	}
	var m Manifest
	if err := json.Unmarshal(w.Body, &m); err != nil {
		return false, err
	}
	if err := m.validate(now); err != nil {
		return false, err
	}
	if m.Org != d.org {
		return false, errors.New("manifest belongs to another organization")
	}
	chain, id, ok := d.cache.Get(w.Fingerprint, now)
	if !ok {
		return false, ErrIdentityNeeded
	}
	if id.Org != m.Org || id.Asset != m.Asset {
		return false, errors.New("manifest identity mismatch")
	}
	if time.UnixMilli(m.Expires).After(id.NotAfter) {
		return false, errors.New("manifest outlives certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return false, err
	}
	input, _ := signingInput(leaf.PublicKey, w.Body)
	valid := false
	switch key := leaf.PublicKey.(type) {
	case ed25519.PublicKey:
		valid = ed25519.Verify(key, input, w.Signature)
	case *ecdsa.PublicKey:
		valid = ecdsa.VerifyASN1(key, input, w.Signature)
	case *rsa.PublicKey:
		valid = rsa.VerifyPKCS1v15(key, crypto.SHA256, input, w.Signature) == nil
	}
	if !valid {
		return false, errors.New("invalid manifest signature")
	}
	h := sha256.Sum256(w.Body)
	hash := hex.EncodeToString(h[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	for asset, e := range d.entries {
		if !e.retainUntil.After(now) {
			delete(d.entries, asset)
		}
	}
	if d.history == nil {
		d.history = map[int32]DirectoryReceipt{}
	}
	if receipt, ok := d.history[m.Asset]; ok && receipt.Until.After(now) {
		if m.Revision < receipt.Revision {
			return false, ErrStaleManifest
		}
		if m.Revision == receipt.Revision && hash != receipt.Hash {
			return false, errors.New("conflicting persisted manifest revision")
		}
	}
	if old, exists := d.entries[m.Asset]; exists {
		if m.Revision < old.manifest.Revision {
			return false, ErrStaleManifest
		}
		if m.Revision == old.manifest.Revision {
			if hash != old.hash {
				return false, errors.New("conflicting manifest revision")
			}
			return false, nil
		}
	} else if len(d.entries) >= d.limit {
		return false, errors.New("directory capacity reached")
	}
	w.Body = append(json.RawMessage(nil), w.Body...)
	w.Signature = append([]byte(nil), w.Signature...)
	next := make(map[int32]DirectoryReceipt, len(d.history)+1)
	for asset, r := range d.history {
		if r.Until.After(now) {
			next[asset] = r
		}
	}
	if _, ok := next[m.Asset]; !ok && len(next) >= d.limit {
		return false, errors.New("directory receipt capacity reached")
	}
	next[m.Asset] = DirectoryReceipt{m.Asset, m.Revision, hash, now.Add(MaxLease + 5*time.Second)}
	if d.persist != nil {
		all := make([]DirectoryReceipt, 0, len(next))
		for _, r := range next {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Asset < all[j].Asset })
		if err := d.persist(all); err != nil {
			return false, err
		}
	}
	d.history = next
	d.entries[m.Asset] = directoryEntry{m, w, hash, now.Add(MaxLease + 5*time.Second)}
	return true, nil
}

// Snapshot returns current origin records. Route reachability is a separate
// filter at the consumer, never inferred from having a directory entry.
func (d *Directory) Snapshot(now time.Time) []Manifest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []Manifest{}
	for _, e := range d.entries {
		m := e.manifest
		if m.Withdraw || !time.UnixMilli(m.Expires).After(now) {
			continue
		}
		if _, _, ok := d.cache.Get(e.wire.Fingerprint, now); !ok {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out
}

// Records includes signed withdrawals for anti-entropy. Expired records are not
// retransmitted; compact revision tombstones still prevent local resurrection.
func (d *Directory) Records(now time.Time) []SignedManifest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []SignedManifest{}
	for _, e := range d.entries {
		if !time.UnixMilli(e.manifest.Expires).After(now) {
			continue
		}
		w := e.wire
		w.Body = append(json.RawMessage(nil), w.Body...)
		w.Signature = append([]byte(nil), w.Signature...)
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i].Fingerprint, string(out[i].Body)) < fmt.Sprint(out[j].Fingerprint, string(out[j].Body))
	})
	return out
}
