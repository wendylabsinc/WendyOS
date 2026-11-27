package evidence

import (
	"bytes"
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
	"time"
	"unicode/utf8"
)

const (
	RetentionProtected = "protected"
	RetentionRolling   = "rolling"
	HoldTask           = "task"
	HoldPackage        = "package"
	MaxPinDigests      = 512
	retentionFile      = ".retention.json"
	publicPinNamespace = "external"
	maxLedgerBytes     = 32 << 20
)

type PinRequest struct {
	ID                string   `json:"id"`
	SHA256            []string `json:"sha256"`
	RetainUntilUnixMS int64    `json:"retain_until_unix_ms"`
}
type UnpinRequest struct {
	ID string `json:"id"`
}
type PinResponse struct {
	ID                string   `json:"id"`
	SHA256            []string `json:"sha256"`
	RetainUntilUnixMS int64    `json:"retain_until_unix_ms"`
	State             string   `json:"state"`
}
type Reference struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}
type pinRecord struct {
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	PinResponse
}
type pinLedger struct {
	Version int                  `json:"version"`
	Tenant  string               `json:"tenant"`
	Site    string               `json:"site"`
	Records map[string]pinRecord `json:"records"`
}

func retentionDefaults(c *Config) error {
	if c.MaxPins == 0 {
		c.MaxPins = 4096
	}
	if c.MaxPinsPerOwner == 0 {
		c.MaxPinsPerOwner = min(128, c.MaxPins)
	}
	if c.MaxPinReferences == 0 {
		c.MaxPinReferences = 16384
	}
	if c.RollingUploadTimeout == 0 {
		c.RollingUploadTimeout = time.Minute
	}
	if c.MaxPins < 1 || c.MaxPins > 10000 || c.MaxPinsPerOwner < 1 || c.MaxPinsPerOwner > c.MaxPins || c.MaxPinReferences < 1 || c.MaxPinReferences > 100000 || c.RollingUploadTimeout < time.Second || c.RollingUploadTimeout > time.Hour {
		return errors.New("invalid evidence retention limits")
	}
	return nil
}
func validManifestRetention(m manifest) bool {
	if m.Version == 1 {
		return m.RetentionClass == "" && m.Allocation == 0 && m.UploadUntilUnixMS == 0
	}
	if m.Allocation == 0 {
		return false
	}
	if m.RetentionClass == RetentionProtected {
		return m.UploadUntilUnixMS == 0
	}
	return m.RetentionClass == RetentionRolling && m.UploadUntilUnixMS > 0 && m.UploadUntilUnixMS <= m.RetainUntilUnixMS
}
func expiredManifest(m manifest, now int64) bool {
	return m.RetainUntilUnixMS <= now || m.RetentionClass == RetentionRolling && !m.Complete && m.UploadUntilUnixMS <= now
}
func validPinName(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func holdNamespace(s string) bool { return s == HoldTask || s == HoldPackage }
func pinKey(namespace, owner, id string) string {
	b, _ := json.Marshal([]string{namespace, owner, id})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func canonicalDigests(input []string) ([]string, error) {
	if len(input) < 1 || len(input) > MaxPinDigests {
		return nil, errors.New("invalid evidence pin digest count")
	}
	out := append([]string(nil), input...)
	sort.Strings(out)
	for i, d := range out {
		if !validDigest(d) || i > 0 && d == out[i-1] {
			return nil, errors.New("invalid or duplicate evidence pin digest")
		}
	}
	return out, nil
}
func copyPin(p PinResponse) PinResponse { p.SHA256 = append([]string(nil), p.SHA256...); return p }
func sameDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (s *Store) clonePins() map[string]pinRecord {
	out := make(map[string]pinRecord, len(s.pins))
	for key, p := range s.pins {
		p.PinResponse = copyPin(p.PinResponse)
		out[key] = p
	}
	return out
}
func (s *Store) pinBounds(records map[string]pinRecord) error {
	if len(records) > s.cfg.MaxPins {
		return ErrQuota
	}
	refs := 0
	owners := map[string]int{}
	for _, p := range records {
		refs += len(p.SHA256)
		key := pinKey(p.Namespace, p.Owner, "owner")
		if p.Namespace == publicPinNamespace {
			owners[key]++
		}
		if refs > s.cfg.MaxPinReferences || owners[key] > s.cfg.MaxPinsPerOwner {
			return ErrQuota
		}
	}
	return nil
}
func (s *Store) rebuildPinned() {
	s.pinned = map[string]int{}
	for _, p := range s.pins {
		if p.State == "active" {
			for _, d := range p.SHA256 {
				s.pinned[d]++
			}
		}
	}
}
func (s *Store) savePins(records map[string]pinRecord) error {
	if err := s.pinBounds(records); err != nil {
		return err
	}
	b, err := json.Marshal(pinLedger{Version: 2, Tenant: s.cfg.Tenant, Site: s.cfg.Site, Records: records})
	if err != nil {
		return err
	}
	if len(b) > maxLedgerBytes {
		return ErrQuota
	}
	if s.pinCommitHook != nil {
		if err = s.pinCommitHook("before-write"); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(s.dir, ".retention-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, retentionFile)); err != nil {
		return err
	}
	if s.pinCommitHook != nil {
		if err = s.pinCommitHook("after-rename"); err != nil {
			return err
		}
	}
	return syncDir(s.dir)
}
func (s *Store) commitPins(records map[string]pinRecord) error {
	if err := s.savePins(records); err != nil {
		if !errors.Is(err, ErrQuota) {
			s.fault = err
		}
		return err
	}
	s.pins = records
	s.rebuildPinned()
	return nil
}
func (s *Store) ensureRetentionMarker() error {
	if _, err := os.Lstat(filepath.Join(s.dir, retentionFile)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		s.fault = err
		return err
	}
	return s.commitPins(s.clonePins())
}
func (s *Store) loadPins() error {
	path := filepath.Join(s.dir, retentionFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	b, err := readRegular(path, maxLedgerBytes)
	if err != nil {
		return err
	}
	var ledger pinLedger
	if err = decodeDisk(b, &ledger); err != nil {
		return err
	}
	if ledger.Version != 2 || ledger.Tenant != s.cfg.Tenant || ledger.Site != s.cfg.Site || ledger.Records == nil {
		return errors.New("invalid evidence retention ledger")
	}
	for key, p := range ledger.Records {
		digests, err := canonicalDigests(p.SHA256)
		if err != nil || !sameDigests(digests, p.SHA256) || !validPinName(p.Owner, 512) || !validPinName(p.ID, 256) || p.RetainUntilUnixMS <= 0 || (p.State != "active" && p.State != "released") || (p.Namespace != publicPinNamespace && !holdNamespace(p.Namespace)) || (p.Namespace != publicPinNamespace && p.State != "active") || key != pinKey(p.Namespace, p.Owner, p.ID) {
			return errors.New("invalid evidence pin record")
		}
	}
	if len(ledger.Records) > 10000 {
		return ErrQuota
	}
	refs := 0
	for _, p := range ledger.Records {
		refs += len(p.SHA256)
	}
	if refs > 100000 {
		return ErrQuota
	}
	s.pins = ledger.Records
	s.rebuildPinned()
	return nil
}
func (s *Store) prunePins() error {
	var next map[string]pinRecord
	now := s.cfg.Now().UnixMilli()
	for key, p := range s.pins {
		if p.RetainUntilUnixMS <= now {
			if next == nil {
				next = s.clonePins()
			}
			delete(next, key)
		}
	}
	if next != nil {
		return s.commitPins(next)
	}
	return nil
}
func (s *Store) validatePins() error {
	if err := s.pinBounds(s.pins); err != nil {
		return err
	}
	for _, p := range s.pins {
		if p.State != "active" {
			continue
		}
		for _, d := range p.SHA256 {
			m, ok := s.entries[d]
			if !ok || !m.Complete || p.RetainUntilUnixMS > m.RetainUntilUnixMS {
				return errors.New("active evidence pin has no complete retained object")
			}
		}
	}
	return nil
}

// makeRoom plans all victims before removing any unexpired object. Active
// uploads, legacy/protected objects and pinned rolling objects are not victims.
func (s *Store) makeRoom(bytes int64, entries int, incoming string) error {
	reserved := int64(0)
	byModality := map[string]int64{}
	victims := []manifest{}
	for _, m := range s.entries {
		reserved += m.Size
		byModality[modality(m.ContentType)] += m.Size
		if m.RetentionClass == RetentionRolling && m.Complete && s.pinned[m.SHA256] == 0 {
			victims = append(victims, m)
		}
	}
	reserved += bytes
	byModality[incoming] += bytes
	count := len(s.entries) + entries
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].Allocation == victims[j].Allocation {
			return victims[i].SHA256 < victims[j].SHA256
		}
		return victims[i].Allocation < victims[j].Allocation
	})
	selected := map[string]bool{}
	choose := func(m manifest) {
		selected[m.SHA256] = true
		reserved -= m.Size
		byModality[modality(m.ContentType)] -= m.Size
		count--
	}
	// Satisfy each modality first. A global candidate cannot repair another
	// modality's deficit. Do not delete any victim until every limit is feasible.
	for _, name := range modalities {
		// An inherited/global-size cap adds no constraint. Preserve legacy
		// global oldest-first eviction when no narrower modality cap applies.
		if s.cfg.ModalityBudgets[name].QuotaBytes == s.cfg.QuotaBytes {
			continue
		}
		for _, m := range victims {
			if byModality[name] <= s.cfg.ModalityBudgets[name].QuotaBytes {
				break
			}
			if modality(m.ContentType) == name {
				choose(m)
			}
		}
		if byModality[name] > s.cfg.ModalityBudgets[name].QuotaBytes {
			return ErrQuota
		}
	}
	for _, m := range victims {
		if reserved <= s.cfg.QuotaBytes && count <= s.cfg.MaxEntries {
			break
		}
		if !selected[m.SHA256] {
			choose(m)
		}
	}
	if reserved > s.cfg.QuotaBytes || count > s.cfg.MaxEntries {
		return ErrQuota
	}
	// The plan is atomic on quota refusal, not a filesystem transaction. An I/O
	// failure can remove a prefix; retain the existing fail-closed recovery rule.
	for _, m := range victims {
		if selected[m.SHA256] {
			if err := s.remove(m.SHA256); err != nil {
				s.fault = err
				return err
			}
		}
	}
	return nil
}

func (s *Store) verifyPinObjects(digests []string, until int64, refs map[string]Reference) (int64, error) {
	now := s.cfg.Now().UnixMilli()
	if until < 0 || until != 0 && until <= now {
		return 0, errors.New("invalid evidence pin expiry")
	}
	effective := until
	for _, d := range digests {
		m, ok := s.entries[d]
		if !ok || !m.Complete || m.RetainUntilUnixMS <= now {
			return 0, ErrUnavailable
		}
		if expected, ok := refs[d]; ok && (m.Size != expected.Size || m.ContentType != expected.ContentType) {
			return 0, errors.New("evidence hold metadata differs")
		}
		if until > m.RetainUntilUnixMS {
			return 0, errors.New("evidence pin exceeds original hard expiry")
		}
		if until == 0 && (effective == 0 || m.RetainUntilUnixMS < effective) {
			effective = m.RetainUntilUnixMS
		}
		if err := s.verify(m); err != nil {
			return 0, fmt.Errorf("evidence pin integrity: %w", err)
		}
	}
	if effective <= s.cfg.Now().UnixMilli() {
		return 0, ErrUnavailable
	}
	return effective, nil
}

// Pin creates an immutable, caller-owned group with an explicit expiry.
// Retrying a released group never reactivates it before that expiry.
func (s *Store) Pin(owner string, r PinRequest) (PinResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return PinResponse{}, err
	}
	if !validPinName(owner, 512) || !validPinName(r.ID, 256) {
		return PinResponse{}, errors.New("invalid evidence pin identity")
	}
	digests, err := canonicalDigests(r.SHA256)
	if err != nil {
		return PinResponse{}, err
	}
	if err = s.prune(); err != nil {
		return PinResponse{}, err
	}
	if r.RetainUntilUnixMS <= s.cfg.Now().UnixMilli() {
		return PinResponse{}, errors.New("invalid evidence pin expiry")
	}
	key := pinKey(publicPinNamespace, owner, r.ID)
	if existing, ok := s.pins[key]; ok {
		if !sameDigests(existing.SHA256, digests) || r.RetainUntilUnixMS != existing.RetainUntilUnixMS {
			return PinResponse{}, errors.New("evidence pin identity already binds another request")
		}
		if existing.State == "active" {
			if _, err = s.verifyPinObjects(digests, existing.RetainUntilUnixMS, nil); err != nil {
				return PinResponse{}, err
			}
		}
		return copyPin(existing.PinResponse), nil
	}
	until, err := s.verifyPinObjects(digests, r.RetainUntilUnixMS, nil)
	if err != nil {
		return PinResponse{}, err
	}
	p := pinRecord{Namespace: publicPinNamespace, Owner: owner, PinResponse: PinResponse{ID: r.ID, SHA256: digests, RetainUntilUnixMS: until, State: "active"}}
	next := s.clonePins()
	next[key] = p
	if err = s.commitPins(next); err != nil {
		return PinResponse{}, err
	}
	if until <= s.cfg.Now().UnixMilli() {
		return PinResponse{}, ErrUnavailable
	}
	return copyPin(p.PinResponse), nil
}

// Unpin preserves a tombstone through the original expiry. Unknown IDs are not
// a cancellation of a future create and return ErrUnavailable.
func (s *Store) Unpin(owner string, r UnpinRequest) (PinResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return PinResponse{}, err
	}
	if !validPinName(owner, 512) || !validPinName(r.ID, 256) {
		return PinResponse{}, errors.New("invalid evidence pin identity")
	}
	if err := s.prune(); err != nil {
		return PinResponse{}, err
	}
	key := pinKey(publicPinNamespace, owner, r.ID)
	p, ok := s.pins[key]
	if !ok {
		return PinResponse{}, ErrUnavailable
	}
	if p.State == "released" {
		return copyPin(p.PinResponse), nil
	}
	p.State = "released"
	next := s.clonePins()
	next[key] = p
	if err := s.commitPins(next); err != nil {
		return PinResponse{}, err
	}
	return copyPin(p.PinResponse), nil
}

// ReplaceHold is only for trusted task/package code. That caller must serialize
// replacement with its reference commit and must not replace a committed result.
// Zero expiry selects the earliest original expiry; no hold extends object life.
func (s *Store) ReplaceHold(namespace, owner, id string, refs []Reference, until int64) (PinResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return PinResponse{}, err
	}
	if !holdNamespace(namespace) || !validPinName(owner, 512) || !validPinName(id, 256) {
		return PinResponse{}, errors.New("invalid evidence hold identity")
	}
	if len(refs) < 1 || len(refs) > MaxPinDigests {
		return PinResponse{}, errors.New("invalid evidence hold reference count")
	}
	digests := make([]string, 0, len(refs))
	expected := map[string]Reference{}
	for _, r := range refs {
		if r.Size < 1 || !validType(r.ContentType) {
			return PinResponse{}, errors.New("invalid evidence hold metadata")
		}
		if previous, exists := expected[r.SHA256]; exists {
			if previous != r {
				return PinResponse{}, errors.New("conflicting duplicate evidence hold reference")
			}
			continue
		}
		digests = append(digests, r.SHA256)
		expected[r.SHA256] = r
	}
	digests, err := canonicalDigests(digests)
	if err != nil {
		return PinResponse{}, err
	}
	if err = s.prune(); err != nil {
		return PinResponse{}, err
	}
	until, err = s.verifyPinObjects(digests, until, expected)
	if err != nil {
		return PinResponse{}, err
	}
	key := pinKey(namespace, owner, id)
	p := pinRecord{Namespace: namespace, Owner: owner, PinResponse: PinResponse{ID: id, SHA256: digests, RetainUntilUnixMS: until, State: "active"}}
	if old, ok := s.pins[key]; ok && old.RetainUntilUnixMS == until && sameDigests(old.SHA256, digests) {
		return copyPin(old.PinResponse), nil
	}
	next := s.clonePins()
	next[key] = p
	if err = s.commitPins(next); err != nil {
		return PinResponse{}, err
	}
	if until <= s.cfg.Now().UnixMilli() {
		return PinResponse{}, ErrUnavailable
	}
	return copyPin(p.PinResponse), nil
}

func decodeDisk(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing evidence disk value")
	}
	return nil
}
func (s *Store) ReleaseHold(namespace, owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	if !holdNamespace(namespace) || !validPinName(owner, 512) || !validPinName(id, 256) {
		return errors.New("invalid evidence hold identity")
	}
	if err := s.prune(); err != nil {
		return err
	}
	key := pinKey(namespace, owner, id)
	if _, ok := s.pins[key]; !ok {
		return nil
	}
	next := s.clonePins()
	delete(next, key)
	return s.commitPins(next)
}

// HoldIDs lists the bounded active internal groups for one trusted owner.
func (s *Store) HoldIDs(namespace, owner string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return nil, err
	}
	if !holdNamespace(namespace) || !validPinName(owner, 512) {
		return nil, errors.New("invalid evidence hold identity")
	}
	if err := s.prune(); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, p := range s.pins {
		if p.Namespace == namespace && p.Owner == owner && p.State == "active" {
			ids = append(ids, p.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
