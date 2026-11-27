package proofarchive

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func encode(v any) []byte    { b, _ := json.Marshal(v); return b }
func key(k world.Key) string { return string(encode(k)) }
func canonical(raw []byte, out any, limit int64) error {
	if len(raw) == 0 || int64(len(raw)) > limit {
		return ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrCorrupt
	}
	if _, e := d.Token(); !errors.Is(e, io.EOF) {
		return ErrCorrupt
	}
	if !bytes.Equal(encode(out), raw) {
		return ErrCorrupt
	}
	return nil
}
func wrap(v any) []byte { raw := encode(v); return encode(checked{digest(raw), raw}) }
func unwrap(raw []byte, out any, limit int64) error {
	var c checked
	if canonical(raw, &c, limit*2+256) != nil || c.SHA256 != digest(c.Data) {
		return ErrCorrupt
	}
	return canonical(c.Data, out, limit)
}
func put(b *bolt.Bucket, k []byte, v any) error { return b.Put(k, wrap(v)) }

func normalize(c Config) (Config, error) {
	if (c.RemoteVerifier == nil) != (c.ValidateRemoteAccepted == nil) {
		return c, ErrDenied
	}
	if c.RecentWindow == 0 {
		c.RecentWindow = 30 * time.Second
	}
	if c.MaxRetention == 0 {
		c.MaxRetention = 7 * 24 * time.Hour
	}
	if c.QuotaBytes == 0 {
		c.QuotaBytes = 128 << 20
	}
	if c.MaxEnvelopeBytes == 0 {
		c.MaxEnvelopeBytes = 384 << 10
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = 4096
	}
	if c.MaxPins == 0 {
		c.MaxPins = 256
	}
	if c.MaxRefsPerPin == 0 {
		c.MaxRefsPerPin = 32
	}
	s := Source{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: "archive-validation", Service: "archive", Component: "wendy.archive.validation.v1", Stream: "archive"}
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || c.Path == "/" || c.Clock == nil || c.Verifier == nil || c.ValidatePin == nil || s.Validate() != nil || c.RecentWindow < time.Second || c.RecentWindow > time.Minute || c.MaxRetention < c.RecentWindow || c.MaxRetention > 30*24*time.Hour || c.QuotaBytes < 1<<20 || c.QuotaBytes > 1<<30 || c.MaxEnvelopeBytes < 16<<10 || c.MaxEnvelopeBytes > 384<<10 || c.MaxEntries < 1 || c.MaxEntries > 4096 || c.MaxPins < 1 || c.MaxPins > 4096 || c.MaxRefsPerPin < 1 || c.MaxRefsPerPin > 64 {
		return c, ErrDenied
	}
	return c, nil
}
func configuration(c Config) string {
	return digest(encode(struct {
		Tenant, Site, Device               string
		RecentWindow, MaxRetention         time.Duration
		QuotaBytes, MaxEnvelopeBytes       int64
		MaxEntries, MaxPins, MaxRefsPerPin int
	}{c.Tenant, c.Site, c.Device, c.RecentWindow, c.MaxRetention, c.QuotaBytes, c.MaxEnvelopeBytes, c.MaxEntries, c.MaxPins, c.MaxRefsPerPin}))
}
func privateFile(path string, nonempty bool) error {
	i, e := os.Lstat(path)
	if e != nil {
		return e
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Geteuid()) || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || nonempty && i.Size() == 0 {
		return ErrCorrupt
	}
	return nil
}
func syncParent(path string) error {
	f, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func openDB(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second, OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
		return os.OpenFile(name, (flags&^os.O_CREATE)|syscall.O_NOFOLLOW, mode)
	}})
}
func newArchive(c Config, db *bolt.DB) *Archive { return &Archive{cfg: c, db: db, commit: db.Update} }
func (a *Archive) scope(r Reference) bool {
	return r.Validate() == nil && r.Key.Tenant == a.cfg.Tenant && r.Key.Site == a.cfg.Site && (r.Key.Source.Device == a.cfg.Device || a.cfg.RemoteVerifier != nil && r.Key.Type == sensorrecord.ComponentType)
}
func (a *Archive) localScope(r Reference) bool {
	return a.scope(r) && r.Key.Source.Device == a.cfg.Device
}
func (a *Archive) inputScope(r Reference) bool {
	return a.scope(r) && r.Key.Type != resultrecord.ComponentType
}

// decodeEnvelope checks immutable projections and shape without asserting
// current authority. First admission additionally requires the live verifier.
func (a *Archive) decodeEnvelope(raw []byte) (serviceorigin.SignedRecord, Reference, error) {
	var s serviceorigin.SignedRecord
	if int64(len(raw)) > a.cfg.MaxEnvelopeBytes || authority.DecodeStrictLimit(raw, &s, int(a.cfg.MaxEnvelopeBytes)) != nil || len(s.Record) == 0 || len(s.Record) > serviceorigin.MaxRecordBytes || len(s.Signature) != 64 || len(s.Delegation.Claims) == 0 || len(s.Delegation.Claims) > serviceorigin.MaxClaimsBytes || len(s.Delegation.Signature) != 3309 || len(s.Delegation.ChainDER) < 1 || len(s.Delegation.ChainDER) > 8 {
		return s, Reference{}, ErrCorrupt
	}
	total := 0
	for _, der := range s.Delegation.ChainDER {
		total += len(der)
		if len(der) == 0 || total > 64<<10 {
			return s, Reference{}, ErrCorrupt
		}
	}
	var r serviceorigin.Record
	var c serviceorigin.DelegationClaims
	if jws.DecodeStrict(s.Record, &r) != nil || jws.DecodeStrict(s.Delegation.Claims, &c) != nil || r.Version != serviceorigin.Version || c.Version != serviceorigin.Version || r.AuthorityDomain != serviceorigin.AuthorityDomain || c.AuthorityDomain != serviceorigin.AuthorityDomain || r.Scope.Validate() != nil || !reflect.DeepEqual(r.Scope, c.Scope) || r.Epoch != c.Epoch || r.Epoch == 0 || r.Sequence == 0 || len(c.PublicKey) != 32 || r.DelegationSHA256 != digest(s.Delegation.Claims) || c.ExpiresAtUnixMS <= c.IssuedAtUnixMS || r.ExpiresAtUnixMS <= r.IssuedAtUnixMS || r.IssuedAtUnixMS < c.IssuedAtUnixMS || r.ExpiresAtUnixMS > c.ExpiresAtUnixMS {
		return s, Reference{}, ErrCorrupt
	}
	ref := Reference{Key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: r.Publication.Entity, Type: r.Publication.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: r.Publication.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, EnvelopeSHA256: digest(raw), RecordSHA256: digest(s.Record)}
	if !a.scope(ref) {
		return s, Reference{}, ErrDenied
	}
	if !a.localScope(ref) && !remoteInline(r) {
		return s, Reference{}, ErrDenied
	}
	return s, ref, nil
}
func (a *Archive) readProof(tx *bolt.Tx, ref Reference) (Descriptor, error) {
	var d Descriptor
	raw := tx.Bucket(proofBucket).Get([]byte(ref.EnvelopeSHA256))
	if raw == nil {
		return d, ErrUnavailable
	}
	if unwrap(raw, &d, a.cfg.MaxEnvelopeBytes*2+16<<10) != nil || d.Admitted.Validate() != nil || d.RecentUntilUnixMS != d.Admitted.Earliest.Add(a.cfg.RecentWindow).UnixMilli() || d.RetainUntilUnixMS < d.RecentUntilUnixMS || d.RetainUntilUnixMS > d.Admitted.Earliest.Add(a.cfg.MaxRetention).UnixMilli() {
		return Descriptor{}, ErrCorrupt
	}
	_, actual, e := a.decodeEnvelope(d.Envelope)
	if e != nil || actual != d.Reference {
		return Descriptor{}, ErrCorrupt
	}
	if d.Reference != ref {
		return Descriptor{}, ErrUnavailable
	}
	return d, nil
}
func (a *Archive) readPin(tx *bolt.Tx, ref Reference) (storedPin, error) {
	var p storedPin
	raw := tx.Bucket(pinBucket).Get([]byte(ref.EnvelopeSHA256))
	if raw == nil {
		return p, ErrUnavailable
	}
	if unwrap(raw, &p, 128<<10) != nil || !a.localScope(p.Claim) || p.Created.Validate() != nil || p.RetainUntilUnixMS <= p.Created.Latest.UnixMilli() || p.RetainUntilUnixMS > p.Created.Earliest.Add(a.cfg.MaxRetention).UnixMilli() || len(p.Inputs) == 0 || len(p.Inputs) > a.cfg.MaxRefsPerPin {
		return storedPin{}, ErrCorrupt
	}
	seen := map[Reference]bool{}
	for _, r := range p.Inputs {
		if !a.inputScope(r) || seen[r] || r == ref {
			return storedPin{}, ErrCorrupt
		}
		seen[r] = true
	}
	if p.Claim != ref {
		return storedPin{}, ErrUnavailable
	}
	return p, nil
}
func water(tx *bolt.Tx, bucket []byte, k world.Key) (Reference, bool, error) {
	var r Reference
	raw := tx.Bucket(bucket).Get([]byte(key(k)))
	if raw == nil {
		return r, false, nil
	}
	if unwrap(raw, &r, 16<<10) != nil || r.Key != k || r.Validate() != nil {
		return Reference{}, false, ErrCorrupt
	}
	return r, true, nil
}
func (a *Archive) usage(tx *bolt.Tx) (usage, error) {
	u := usage{}
	for _, b := range []struct {
		name  []byte
		count *int
	}{{proofBucket, &u.Entries}, {pinBucket, &u.Pins}, {rememberBucket, &u.RememberKeys}, {pinWaterBucket, &u.PinKeys}} {
		bucket := tx.Bucket(b.name)
		if bucket == nil {
			return u, ErrCorrupt
		}
		if e := bucket.ForEach(func(k, v []byte) error {
			if v == nil {
				return ErrCorrupt
			}
			*b.count++
			u.Bytes += int64(len(k) + len(v))
			return nil
		}); e != nil {
			return u, e
		}
	}
	if u.Bytes > a.cfg.QuotaBytes || u.Entries > a.cfg.MaxEntries || u.Pins > a.cfg.MaxPins || u.RememberKeys > a.cfg.MaxEntries || u.PinKeys > a.cfg.MaxEntries {
		return u, ErrQuota
	}
	return u, nil
}

func Create(c Config) (a *Archive, err error) {
	c, err = normalize(c)
	if err != nil {
		return nil, err
	}
	w, err := c.Clock.Now()
	if err != nil || w.Validate() != nil {
		return nil, ErrClock
	}
	if err = hostpath.EnsureDirectory(filepath.Dir(c.Path), 0700, true); err != nil {
		return nil, err
	}
	for _, p := range []string{c.Path, c.Path + ".installation"} {
		if _, e := os.Lstat(p); e == nil || !errors.Is(e, os.ErrNotExist) {
			return nil, ErrCorrupt
		}
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	m := marker{1, hex.EncodeToString(random[:]), configuration(c)}
	f, err := os.OpenFile(c.Path+".installation", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(encode(m))
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return nil, err
	}
	f, err = os.OpenFile(c.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := openDB(c.Path)
	if err != nil {
		return nil, err
	}
	a = newArchive(c, db)
	a.floor = w
	a.observed = w
	defer func() {
		if err != nil {
			db.Close()
			a = nil
		}
	}()
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{metaBucket, proofBucket, pinBucket, rememberBucket, pinWaterBucket} {
			if _, e := tx.CreateBucket(b); e != nil {
				return e
			}
		}
		if e := put(tx.Bucket(metaBucket), markerKey, m); e != nil {
			return e
		}
		if e := put(tx.Bucket(metaBucket), floorKey, w); e != nil {
			return e
		}
		return put(tx.Bucket(metaBucket), usageKey, usage{})
	})
	if err != nil {
		return nil, err
	}
	if err = syncParent(c.Path); err != nil {
		return nil, err
	}
	if _, err = a.now(context.Background()); err != nil {
		return nil, err
	}
	return a, nil
}

// Open validates historical bytes and persisted admission metadata. Expired
// source certificates do not prevent opening or authorize current publication.
func Open(c Config) (a *Archive, err error) {
	c, err = normalize(c)
	if err != nil {
		return nil, err
	}
	if err = hostpath.EnsureDirectory(filepath.Dir(c.Path), 0700, false); err != nil {
		return nil, err
	}
	for _, p := range []string{c.Path, c.Path + ".installation"} {
		if err = privateFile(p, true); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNotInitialized
			}
			return nil, err
		}
	}
	f, err := os.OpenFile(c.Path+".installation", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	raw, e := io.ReadAll(io.LimitReader(f, 2049))
	f.Close()
	if e != nil {
		return nil, e
	}
	var m marker
	if canonical(raw, &m, 2048) != nil || m.Version != 1 || len(m.InstallationID) != 64 || m.ConfigurationSHA256 != configuration(c) {
		return nil, ErrCorrupt
	}
	id, decodeErr := hex.DecodeString(m.InstallationID)
	if decodeErr != nil || len(id) != 32 || hex.EncodeToString(id) != m.InstallationID {
		return nil, ErrCorrupt
	}
	db, err := openDB(c.Path)
	if err != nil {
		return nil, err
	}
	a = newArchive(c, db)
	defer func() {
		if err != nil {
			db.Close()
			a = nil
		}
	}()
	err = db.View(func(tx *bolt.Tx) error {
		known := map[string]bool{}
		for _, b := range [][]byte{metaBucket, proofBucket, pinBucket, rememberBucket, pinWaterBucket} {
			known[string(b)] = true
			if tx.Bucket(b) == nil {
				return ErrCorrupt
			}
		}
		if e := tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			if !known[string(name)] {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		if e := tx.Bucket(metaBucket).ForEach(func(k, v []byte) error {
			if v == nil || !bytes.Equal(k, markerKey) && !bytes.Equal(k, floorKey) && !bytes.Equal(k, usageKey) {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		var inside marker
		var stored usage
		if unwrap(tx.Bucket(metaBucket).Get(markerKey), &inside, 2048) != nil || inside != m || unwrap(tx.Bucket(metaBucket).Get(floorKey), &a.floor, 4096) != nil || a.floor.Validate() != nil || unwrap(tx.Bucket(metaBucket).Get(usageKey), &stored, 1024) != nil {
			return ErrCorrupt
		}
		actual, e := a.usage(tx)
		if e != nil || stored != actual {
			return ErrCorrupt
		}
		for _, b := range [][]byte{rememberBucket, pinWaterBucket} {
			if e := tx.Bucket(b).ForEach(func(k, v []byte) error {
				var r Reference
				if unwrap(v, &r, 16<<10) != nil || !a.scope(r) || bytes.Equal(b, pinWaterBucket) && !a.localScope(r) || string(k) != key(r.Key) {
					return ErrCorrupt
				}
				return nil
			}); e != nil {
				return e
			}
		}
		if e := tx.Bucket(proofBucket).ForEach(func(k, v []byte) error {
			var d Descriptor
			if unwrap(v, &d, c.MaxEnvelopeBytes*2+16<<10) != nil || string(k) != d.Reference.EnvelopeSHA256 {
				return ErrCorrupt
			}
			d, e := a.readProof(tx, d.Reference)
			if e != nil {
				return e
			}
			if d.Admitted.Latest.After(a.floor.Latest) || d.Admitted.Earliest.After(a.floor.Earliest) {
				return ErrCorrupt
			}
			h, ok, e := water(tx, rememberBucket, d.Reference.Key)
			if e != nil || !ok || h.Version.Compare(d.Reference.Version) < 0 || h.Version == d.Reference.Version && h != d.Reference {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		return tx.Bucket(pinBucket).ForEach(func(k, v []byte) error {
			var p storedPin
			if unwrap(v, &p, 128<<10) != nil || string(k) != p.Claim.EnvelopeSHA256 {
				return ErrCorrupt
			}
			p, e := a.readPin(tx, p.Claim)
			if e != nil {
				return e
			}
			if p.Created.Latest.After(a.floor.Latest) || p.Created.Earliest.After(a.floor.Earliest) {
				return ErrCorrupt
			}
			h, ok, e := water(tx, pinWaterBucket, p.Claim.Key)
			if e != nil || !ok || h.Version.Compare(p.Claim.Version) < 0 || h.Version == p.Claim.Version && h != p.Claim {
				return ErrCorrupt
			}
			for _, r := range p.Inputs {
				d, e := a.readProof(tx, r)
				if e != nil || d.RetainUntilUnixMS < p.RetainUntilUnixMS {
					return ErrCorrupt
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if _, err = a.now(context.Background()); err != nil {
		return nil, err
	}
	// Persist this boot's physical floor before exposing any stored bytes.
	_, err = a.transaction(context.Background(), func(*bolt.Tx, trustedtime.Interval) error { return nil })
	if err != nil {
		return nil, err
	}
	return a, nil
}
