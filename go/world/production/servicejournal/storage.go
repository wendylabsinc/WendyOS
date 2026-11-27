package servicejournal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func digest(b []byte) string           { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func encode(v any) ([]byte, error)     { return json.Marshal(v) }
func checked(data []byte) checkedBytes { return checkedBytes{SHA256: digest(data), Data: data} }
func decodeCanonical(b []byte, out any, limit int64) error {
	if len(b) == 0 || int64(len(b)) > limit {
		return ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrCorrupt
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	canonical, err := encode(out)
	if err != nil || !bytes.Equal(canonical, b) {
		return ErrCorrupt
	}
	return nil
}
func decodeChecked(b []byte, out any, limit int64) error {
	var envelope checkedBytes
	if decodeCanonical(b, &envelope, limit*2+256) != nil || envelope.SHA256 != digest(envelope.Data) {
		return ErrCorrupt
	}
	return decodeCanonical(envelope.Data, out, limit)
}
func putChecked(bucket *bolt.Bucket, key []byte, v any) error {
	raw, err := encode(v)
	if err != nil {
		return err
	}
	wrapped, err := encode(checked(raw))
	if err != nil {
		return err
	}
	return bucket.Put(key, wrapped)
}

func normalize(cfg Config) (Config, error) {
	if cfg.Path == "" || cfg.Verifier == nil || cfg.PhysicalClock == nil || cfg.AdmitSchema == nil || cfg.MaxEntries < 1 || cfg.MaxEntries > 4096 || cfg.MaxProofBytes < 64<<10 || cfg.MaxProofBytes > 64<<20 || cfg.Replica.MaxEntries != cfg.MaxEntries {
		return cfg, ErrCapacity
	}
	if _, err := world.NewReplica(cfg.Replica); err != nil {
		return cfg, err
	}
	path, err := filepath.Abs(cfg.Path)
	if err != nil {
		return cfg, err
	}
	cfg.Path = path
	cfg.Replica.Frames = append([]world.FrameRef(nil), cfg.Replica.Frames...)
	return cfg, nil
}
func configDigest(cfg Config) string {
	frames := append([]world.FrameRef(nil), cfg.Replica.Frames...)
	sort.Slice(frames, func(i, j int) bool {
		if frames[i].ID == frames[j].ID {
			return frames[i].Revision < frames[j].Revision
		}
		return frames[i].ID < frames[j].ID
	})
	b, _ := encode(struct {
		Tenant, Site, ClockDomain string
		MaxFutureSkew             time.Duration
		AllowSynthetic            bool
		Frames                    []world.FrameRef
		MaxEntries                int
		MaxProofBytes             int64
	}{cfg.Replica.Tenant, cfg.Replica.Site, cfg.Replica.ClockDomain, cfg.Replica.MaxFutureSkew, cfg.Replica.AllowSynthetic, frames, cfg.MaxEntries, cfg.MaxProofBytes})
	return digest(b)
}
func privateFile(path string, nonempty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || nonempty && info.Size() == 0 {
		return ErrCorrupt
	}
	return nil
}
func privateParent(path string, create bool) error {
	return hostpath.EnsureDirectory(filepath.Dir(path), 0700, create)
}
func syncParent(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func openDB(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second, OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
		return os.OpenFile(name, (flags&^os.O_CREATE)|syscall.O_NOFOLLOW, mode)
	}})
}
func newJournal(cfg Config, db *bolt.DB) *Journal {
	return &Journal{cfg: cfg, db: db, proofs: map[string]storedProof{}, epochs: map[string]uint64{}, commit: db.Update}
}

// Create is explicit first commissioning. A marker or database that already
// exists is never replaced. Interrupted initialization remains fail-closed for
// operator recovery; Open never silently creates a new source-epoch history.
func Create(cfg Config) (j *Journal, err error) {
	return createJournal(cfg, "")
}

func createJournal(cfg Config, remoteDevice string) (j *Journal, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if err = privateParent(cfg.Path, true); err != nil {
		return nil, err
	}
	if _, e := os.Lstat(cfg.Path); e == nil || !errors.Is(e, os.ErrNotExist) {
		return nil, ErrCorrupt
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	m := marker{Version: 1, InstallationID: hex.EncodeToString(random[:]), ConfigurationSHA256: configDigest(cfg)}
	if remoteDevice != "" {
		m.Version, m.ConfigurationSHA256 = 2, remoteConfigDigest(cfg, remoteDevice)
	}
	markerBytes, _ := encode(m)
	f, err := os.OpenFile(cfg.Path+".installation", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(markerBytes)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	f, err = os.OpenFile(cfg.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := openDB(cfg.Path)
	if err != nil {
		return nil, err
	}
	j = newJournal(cfg, db)
	j.remoteDevice = remoteDevice
	defer func() {
		if err != nil {
			db.Close()
			j = nil
		}
	}()
	j.replica, err = world.NewReplica(cfg.Replica)
	if err != nil {
		return nil, err
	}
	checkpoint, err := j.replica.Checkpoint()
	if err != nil {
		return nil, err
	}
	j.floor, err = cfg.PhysicalClock.Now()
	if err != nil || j.floor.Validate() != nil {
		return nil, serviceorigin.ErrClock
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{metaBucket, proofsBucket, epochsBucket} {
			if _, e := tx.CreateBucket(name); e != nil {
				return e
			}
		}
		meta := tx.Bucket(metaBucket)
		if e := putChecked(meta, markerKey, m); e != nil {
			return e
		}
		if e := putChecked(meta, checkpointKey, checkpoint); e != nil {
			return e
		}
		return putChecked(meta, floorKey, j.floor)
	})
	if err != nil {
		return nil, err
	}
	if err = syncParent(cfg.Path); err != nil {
		return nil, err
	}
	return j, nil
}

// Open validates historical storage without granting current authority to
// expired proofs. Its checksums assume trusted local storage; a rollback of the
// entire volume and marker requires an external monotonic anchor to detect.
func Open(cfg Config) (j *Journal, err error) {
	return openJournal(cfg, "")
}

func openJournal(cfg Config, remoteDevice string) (j *Journal, err error) {
	cfg, err = normalize(cfg)
	if err != nil {
		return nil, err
	}
	if err = privateParent(cfg.Path, false); err != nil {
		return nil, err
	}
	for _, path := range []string{cfg.Path, cfg.Path + ".installation"} {
		if err = privateFile(path, true); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNotInitialized
			}
			return nil, err
		}
	}
	f, err := os.OpenFile(cfg.Path+".installation", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	markerBytes, readErr := io.ReadAll(io.LimitReader(f, 2049))
	f.Close()
	if readErr != nil {
		return nil, readErr
	}
	var m marker
	version, expectedDigest := 1, configDigest(cfg)
	if remoteDevice != "" {
		version, expectedDigest = 2, remoteConfigDigest(cfg, remoteDevice)
	}
	if decodeCanonical(markerBytes, &m, 2048) != nil || m.Version != version || len(m.InstallationID) != 64 || m.ConfigurationSHA256 != expectedDigest {
		return nil, ErrCorrupt
	}
	db, err := openDB(cfg.Path)
	if err != nil {
		return nil, err
	}
	j = newJournal(cfg, db)
	j.remoteDevice = remoteDevice
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: invalid database structure", ErrCorrupt)
		}
		if err != nil {
			db.Close()
			j = nil
		}
	}()
	err = db.View(func(tx *bolt.Tx) error { return j.restore(tx, m) })
	if err != nil {
		return nil, err
	}
	if _, err = j.physicalNow(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Journal) restore(tx *bolt.Tx, expected marker) error {
	if tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		if !bytes.Equal(name, metaBucket) && !bytes.Equal(name, proofsBucket) && !bytes.Equal(name, epochsBucket) {
			return ErrCorrupt
		}
		return nil
	}) != nil {
		return ErrCorrupt
	}
	meta, proofs, epochs := tx.Bucket(metaBucket), tx.Bucket(proofsBucket), tx.Bucket(epochsBucket)
	if meta == nil || proofs == nil || epochs == nil {
		return ErrCorrupt
	}
	if meta.ForEach(func(key, value []byte) error {
		if value == nil || !bytes.Equal(key, markerKey) && !bytes.Equal(key, checkpointKey) && !bytes.Equal(key, floorKey) {
			return ErrCorrupt
		}
		return nil
	}) != nil {
		return ErrCorrupt
	}
	var actual marker
	if decodeChecked(meta.Get(markerKey), &actual, 2048) != nil || actual != expected {
		return ErrCorrupt
	}
	if decodeChecked(meta.Get(floorKey), &j.floor, 4096) != nil || j.floor.Validate() != nil {
		return ErrCorrupt
	}
	var checkpoint world.Checkpoint
	if decodeChecked(meta.Get(checkpointKey), &checkpoint, j.cfg.MaxProofBytes) != nil || len(checkpoint.Entries) > j.cfg.MaxEntries {
		return ErrCorrupt
	}
	if err := proofs.ForEach(func(k, v []byte) error {
		if v == nil || len(j.proofs) >= j.cfg.MaxEntries {
			return ErrCorrupt
		}
		var proof storedProof
		if decodeCanonical(v, &proof, maxEnvelopeBytes*2) != nil || len(proof.Envelope) > maxEnvelopeBytes || proof.SHA256 != digest(proof.Envelope) {
			return ErrCorrupt
		}
		if _, _, err := j.decodeStored(proof.Envelope); err != nil {
			return err
		}
		j.proofBytes += int64(len(proof.Envelope))
		if j.proofBytes > j.cfg.MaxProofBytes {
			return ErrCapacity
		}
		j.proofs[string(k)] = proof
		return nil
	}); err != nil {
		return err
	}
	if len(j.proofs) != len(checkpoint.Entries) {
		return ErrCorrupt
	}
	for _, entry := range checkpoint.Entries {
		key := componentKey(entry.Component.Key)
		proof, ok := j.proofs[key]
		if !ok {
			return ErrCorrupt
		}
		_, record, err := j.decodeStored(proof.Envelope)
		if err != nil {
			return err
		}
		projection := serviceorigin.VerifiedRecord{Record: record}.Component()
		if j.remoteDevice != "" {
			deadline, e := originalDeadline(projection)
			if e != nil || record.Scope.Device == j.remoteDevice || entry.ExpiresAt.After(deadline) {
				return ErrCorrupt
			}
		}
		a, _ := encode(projection)
		b, _ := encode(entry.Component)
		if !bytes.Equal(a, b) {
			return ErrCorrupt
		}
	}
	if err := epochs.ForEach(func(k, v []byte) error {
		if v == nil || len(j.epochs) >= j.cfg.MaxEntries {
			return ErrCorrupt
		}
		var s source
		if decodeCanonical(k, &s, 4096) != nil || validateSource(s, j.cfg.Replica.Site) != nil || s.Tenant != j.cfg.Replica.Tenant {
			return ErrCorrupt
		}
		if j.remoteDevice != "" && s.Device == j.remoteDevice {
			return ErrCorrupt
		}
		var stored storedEpoch
		if decodeCanonical(v, &stored, 256) != nil || stored.Epoch == 0 || stored.SHA256 != epochDigest(string(k), stored.Epoch) {
			return ErrCorrupt
		}
		j.epochs[string(k)] = stored.Epoch
		return nil
	}); err != nil {
		return err
	}
	for _, proof := range j.proofs {
		_, record, err := j.decodeStored(proof.Envelope)
		if err != nil {
			return err
		}
		if j.epochs[sourceKey(fromScope(record.Scope))] < record.Epoch {
			return ErrCorrupt
		}
	}
	r, err := world.Restore(j.cfg.Replica, checkpoint)
	if err != nil {
		return err
	}
	j.replica = r
	return nil
}

func decodeProof(raw []byte) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	if decodeCanonical(raw, &signed, maxEnvelopeBytes) != nil || len(signed.Record) > serviceorigin.MaxRecordBytes || jws.DecodeStrict(signed.Record, &record) != nil || record.Version != serviceorigin.Version || record.Scope.Validate() != nil || record.AuthorityDomain != serviceorigin.AuthorityDomain || record.DelegationSHA256 != digest(signed.Delegation.Claims) || record.Epoch == 0 || record.Sequence == 0 || len(signed.Signature) != 64 {
		return signed, record, ErrCorrupt
	}
	return signed, record, nil
}

func componentKey(k world.Key) string        { b, _ := encode(k); return string(b) }
func fromScope(s serviceorigin.Scope) source { return source{s.Tenant, s.Device, s.App, s.Service} }
func sourceKey(s source) string              { b, _ := encode(s); return string(b) }
func epochDigest(key string, epoch uint64) string {
	b, _ := encode(struct {
		Source string
		Epoch  uint64
	}{key, epoch})
	return digest(b)
}
func validateSource(s source, site string) error {
	return (serviceorigin.Scope{Tenant: s.Tenant, Site: site, Device: s.Device, App: s.App, Service: s.Service, DeploymentGeneration: 1, ExecutionDomain: "physical", Publish: []serviceorigin.PublishScope{{ComponentType: "validation", Stream: "validation"}}}).Validate()
}

func (j *Journal) physicalNow() (trustedtime.Interval, error) {
	w, err := j.cfg.PhysicalClock.Now()
	if err != nil || w.Validate() != nil {
		return trustedtime.Interval{}, serviceorigin.ErrClock
	}
	for _, floor := range []trustedtime.Interval{j.floor, j.observed} {
		if !floor.Earliest.IsZero() && (w.Earliest.Before(floor.Earliest) || w.Latest.Before(floor.Latest) || w.BootID == floor.BootID && w.Monotonic < floor.Monotonic) {
			return trustedtime.Interval{}, serviceorigin.ErrClock
		}
	}
	j.observed = w
	return w, nil
}
