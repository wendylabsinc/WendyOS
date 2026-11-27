package servicejournal

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func remoteConfigDigest(cfg Config, device string) string {
	b, _ := encode(struct{ Purpose, LocalDevice, LocalConfiguration string }{"remote-service-register-v1", device, configDigest(cfg)})
	return digest(b)
}
func remoteConfig(cfg RemoteConfig) (Config, error) {
	if cfg.Guard == nil || cfg.Authorize == nil || validateSource(source{cfg.Replica.Tenant, cfg.LocalDevice, "journal", "remote"}, cfg.Replica.Site) != nil {
		return Config{}, serviceorigin.ErrDenied
	}
	return normalize(Config{Path: cfg.Path, Replica: cfg.Replica, Verifier: cfg.Verifier, PhysicalClock: cfg.PhysicalClock, AdmitSchema: cfg.AdmitSchema, MaxEntries: cfg.MaxEntries, MaxProofBytes: cfg.MaxProofBytes})
}
func CreateRemote(cfg RemoteConfig) (*RemoteJournal, error) {
	base, err := remoteConfig(cfg)
	if err != nil {
		return nil, err
	}
	j, err := createJournal(base, cfg.LocalDevice)
	if err != nil {
		return nil, err
	}
	return &RemoteJournal{base: j, cfg: cfg}, nil
}
func OpenRemote(cfg RemoteConfig) (*RemoteJournal, error) {
	base, err := remoteConfig(cfg)
	if err != nil {
		return nil, err
	}
	j, err := openJournal(base, cfg.LocalDevice)
	if err != nil {
		return nil, err
	}
	return &RemoteJournal{base: j, cfg: cfg}, nil
}
func (r *RemoteJournal) Close() error {
	if r == nil {
		return nil
	}
	return r.base.Close()
}

func decodeRemoteProof(raw []byte) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	var s serviceorigin.SignedRecord
	var record serviceorigin.Record
	if len(raw) == 0 || len(raw) > maxEnvelopeBytes || authority.DecodeStrictLimit(raw, &s, maxEnvelopeBytes) != nil || !envelopeBounds(s) || jws.DecodeStrict(s.Record, &record) != nil || record.Version != serviceorigin.Version || record.Scope.Validate() != nil || record.AuthorityDomain != serviceorigin.AuthorityDomain || record.DelegationSHA256 != digest(s.Delegation.Claims) || record.Epoch == 0 || record.Sequence == 0 {
		return s, record, ErrCorrupt
	}
	return s, record, nil
}
func (j *Journal) decodeStored(raw []byte) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	if j.remoteDevice != "" {
		return decodeRemoteProof(raw)
	}
	return decodeProof(raw)
}

func guarded(ctx context.Context, guard AuthorityGuard, fn func() error) error {
	if ctx == nil || guard == nil {
		return serviceorigin.ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	called := false
	var callbackErr error
	err := guard(ctx, func() error {
		if called {
			callbackErr = serviceorigin.ErrDenied
			return callbackErr
		}
		called = true
		callbackErr = fn()
		return callbackErr
	})
	if !called {
		return serviceorigin.ErrDenied
	}
	if callbackErr != nil {
		return callbackErr
	}
	if err == nil {
		return ctx.Err()
	}
	return err
}
func windowCurrent(w AuthorityWindow, now trustedtime.Interval) bool {
	return now.Validate() == nil && !w.NotBefore.IsZero() && w.NotAfter.After(w.NotBefore) && !now.Earliest.Before(w.NotBefore) && now.Latest.Before(w.NotAfter)
}
func recordCurrent(v serviceorigin.VerifiedRecord, w trustedtime.Interval) bool {
	return windowCurrent(AuthorityWindow{time.UnixMilli(v.Record.IssuedAtUnixMS), time.UnixMilli(v.Record.ExpiresAtUnixMS)}, w) && windowCurrent(AuthorityWindow{time.UnixMilli(v.Delegation.IssuedAtUnixMS), time.UnixMilli(v.Delegation.ExpiresAtUnixMS)}, w) && !w.Earliest.Before(v.Device.ValidFrom) && w.Latest.Before(v.Device.ValidUntil)
}
func ownedVerified(v serviceorigin.VerifiedRecord) (serviceorigin.VerifiedRecord, error) {
	out := v
	out.Record = serviceorigin.Record{}
	out.Delegation = serviceorigin.DelegationClaims{}
	if jws.DecodeStrict(v.Bytes, &out.Record) != nil {
		return out, ErrCorrupt
	}
	b, err := encode(v.Delegation)
	if err != nil || jws.DecodeStrict(b, &out.Delegation) != nil {
		return out, ErrCorrupt
	}
	out.Bytes = bytes.Clone(v.Bytes)
	return out, nil
}
func (r *RemoteJournal) authorize(ctx context.Context, v serviceorigin.VerifiedRecord, w trustedtime.Interval) (AuthorityWindow, error) {
	if !recordCurrent(v, w) {
		rpctrace.Add(ctx, rpctrace.StageRemoteRecordBoundOmitted, 0)
		return AuthorityWindow{}, serviceorigin.ErrExpired
	}
	copy, err := ownedVerified(v)
	if err != nil {
		return AuthorityWindow{}, err
	}
	window, err := r.cfg.Authorize(ctx, copy, w)
	if err != nil {
		return AuthorityWindow{}, err
	}
	if !windowCurrent(window, w) {
		rpctrace.Add(ctx, rpctrace.StageRemoteCallbackWindowOmitted, 0)
		return AuthorityWindow{}, serviceorigin.ErrExpired
	}
	return window, ctx.Err()
}
func (r *RemoteJournal) verify(ctx context.Context, raw []byte) (serviceorigin.VerifiedRecord, error) {
	signed, record, err := decodeRemoteProof(raw)
	if err != nil {
		return serviceorigin.VerifiedRecord{}, err
	}
	if record.Scope.Device == r.cfg.LocalDevice {
		return serviceorigin.VerifiedRecord{}, serviceorigin.ErrDenied
	}
	v, err := r.base.verify(ctx, signed)
	if err != nil {
		return v, err
	}
	if !bytes.Equal(v.Bytes, signed.Record) || !reflect.DeepEqual(v.Record, record) {
		return v, ErrCorrupt
	}
	return v, nil
}

func originalDeadline(c world.Component) (time.Time, error) {
	if c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.TTL <= 0 {
		return time.Time{}, world.ErrUnknownFreshness
	}
	n := c.Timing.SampleTime.UnixNano()
	if n <= 0 || !time.Unix(0, n).Equal(c.Timing.SampleTime) {
		return time.Time{}, world.ErrUnknownFreshness
	}
	deadline := c.Timing.SampleTime.Add(c.TTL - max(c.Timing.ErrorBound, c.Timing.AgeHint))
	n = deadline.UnixNano()
	if n <= 0 || !time.Unix(0, n).Equal(deadline) {
		return time.Time{}, world.ErrUnknownFreshness
	}
	return deadline, nil
}

// recordWindowRefusal identifies an original authority-window refusal before
// this record's transaction can commit. Other read/clock metadata may have
// changed; this is not a claim that the whole operation made no writes.
type recordWindowRefusal struct{ cause error }

func (e *recordWindowRefusal) Error() string {
	return "record window refused before commit: " + e.cause.Error()
}
func (e *recordWindowRefusal) Unwrap() error { return e.cause }

// IsRecordWindowRefusal is true only for the journal's explicit precommit
// window refusal. A failed commit or any later fault cannot carry this result.
func IsRecordWindowRefusal(err error) bool {
	var refused *recordWindowRefusal
	return errors.As(err, &refused) && !errors.Is(err, ErrFaulted) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
func descriptor(raw []byte, record serviceorigin.Record, expiry time.Time) RecordDescriptor {
	c := (serviceorigin.VerifiedRecord{Record: record}).Component()
	return RecordDescriptor{Reference: proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: digest(raw), RecordSHA256: digest(rawRecord(raw))}, ExpiresAtUnixNS: expiry.UnixNano(), Tombstone: c.Tombstone}
}
func rawRecord(raw []byte) []byte { s, _, _ := decodeRemoteProof(raw); return s.Record }
func validateDescriptor(d RecordDescriptor, tenant, site, localDevice string) error {
	if d.Reference.Validate() != nil || d.Reference.Key.Tenant != tenant || d.Reference.Key.Site != site || d.Reference.Key.Source.Device == localDevice || d.ExpiresAtUnixNS <= 0 {
		return serviceorigin.ErrDenied
	}
	return nil
}

// AdmitRemote accepts the exact original JSON envelope and an unsigned source-
// clock deadline cap. The cap can only shorten the original lifetime. Tombstones
// and already expired samples still repair version floors while original signer
// authority is valid. finalCheck is mandatory and must do local work only.
func (r *RemoteJournal) AdmitRemote(ctx context.Context, envelope []byte, expiryCapUnixNS int64, finalCheck func(context.Context) error) (result world.Result, err error) {
	if r == nil || ctx == nil || finalCheck == nil {
		return "", serviceorigin.ErrDenied
	}
	if r.faulted.Load() {
		return "", ErrFaulted
	}
	if len(envelope) == 0 || len(envelope) > maxEnvelopeBytes || expiryCapUnixNS <= 0 {
		return "", ErrCapacity
	}
	raw := bytes.Clone(envelope)
	committed := false
	err = guarded(ctx, r.cfg.Guard, func() error {
		j := r.base
		j.lock(ctx)
		defer j.mu.Unlock()
		windowRefusal := func(e error) error {
			// Only this exact known window error is deferrable. Clock, storage,
			// schema, policy and arbitrary callback errors keep their old outcome.
			if e == serviceorigin.ErrExpired && j.usable() == nil && !r.faulted.Load() {
				return &recordWindowRefusal{cause: e}
			}
			return e
		}
		if e := j.usable(); e != nil {
			return e
		}
		if r.faulted.Load() {
			return ErrFaulted
		}
		v, e := r.verify(ctx, raw)
		if e != nil {
			return windowRefusal(e)
		}
		w, e := j.current(ctx)
		if e != nil {
			return e
		}
		if _, e = r.authorize(ctx, v, w); e != nil {
			return windowRefusal(e)
		}
		c := v.Component()
		original, e := originalDeadline(c)
		if e != nil {
			return e
		}
		cap := time.Unix(0, expiryCapUnixNS)
		if original.Before(cap) {
			cap = original
		}
		key := componentKey(c.Key)
		actor := sourceKey(fromScope(v.Record.Scope))
		old, exists := j.proofs[key]
		if v.Record.Epoch < j.epochs[actor] {
			return ErrEpoch
		}
		if exists {
			_, prior, e := decodeRemoteProof(old.Envelope)
			if e != nil {
				return j.fault(e)
			}
			if prior.Epoch == v.Record.Epoch && prior.Sequence == v.Record.Sequence && !bytes.Equal(old.Envelope, raw) {
				return world.ErrConflict
			}
		}
		candidate, e := j.replica.Clone()
		if e != nil {
			return e
		}
		result, e = candidate.Apply(c.Key.Source, c)
		if e != nil {
			return e
		}
		if e = candidate.ClampExpiry(c.Key, c.Version, cap); e != nil {
			return e
		}
		if j.epochs[actor] == 0 && len(j.epochs) >= j.cfg.MaxEntries {
			return ErrCapacity
		}
		total := j.proofBytes
		if result == world.Applied {
			if !exists && len(j.proofs) >= j.cfg.MaxEntries {
				return ErrCapacity
			}
			total += int64(len(raw)) - int64(len(old.Envelope))
			if total > j.cfg.MaxProofBytes {
				return ErrCapacity
			}
		}
		checkpoint, e := candidate.Checkpoint()
		if e != nil {
			return e
		}
		b, e := encode(checkpoint)
		if e != nil || int64(len(b)) > j.cfg.MaxProofBytes {
			return ErrCapacity
		}
		proof := storedProof{SHA256: digest(raw), Envelope: raw}
		epoch := max(j.epochs[actor], v.Record.Epoch)
		var floor trustedtime.Interval
		committing := false
		e = j.commitTrace(ctx, func(tx *bolt.Tx) error {
			checked, e := r.verify(ctx, raw)
			if e != nil {
				return windowRefusal(e)
			}
			w, e := j.current(ctx)
			if e != nil {
				return e
			}
			grant, e := r.authorize(ctx, checked, w)
			if e != nil {
				return windowRefusal(e)
			}
			if e = finalCheck(ctx); e != nil {
				return e
			}
			floor, e = j.current(ctx)
			if e != nil {
				return e
			}
			if !windowCurrent(grant, floor) || !recordCurrent(checked, floor) {
				return windowRefusal(serviceorigin.ErrExpired)
			}
			if result == world.Applied {
				b, e := encode(proof)
				if e != nil {
					return e
				}
				if e = tx.Bucket(proofsBucket).Put([]byte(key), b); e != nil {
					return e
				}
			}
			b, e := encode(storedEpoch{Epoch: epoch, SHA256: epochDigest(actor, epoch)})
			if e != nil {
				return e
			}
			if e = tx.Bucket(epochsBucket).Put([]byte(actor), b); e != nil {
				return e
			}
			if e = putChecked(tx.Bucket(metaBucket), checkpointKey, checkpoint); e != nil {
				return e
			}
			if e = putChecked(tx.Bucket(metaBucket), floorKey, floor); e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			committing = true
			return nil
		})
		if e != nil {
			if committing {
				return j.fault(e)
			}
			return e
		}
		committed = true
		j.replica = candidate
		j.floor = floor
		j.epochs[actor] = epoch
		if result == world.Applied {
			j.proofs[key] = proof
			j.proofBytes = total
		}
		v, e = r.verify(ctx, raw)
		if e != nil {
			return j.fault(e)
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		grant, e := r.authorize(ctx, v, w)
		if e != nil {
			return j.fault(e)
		}
		if e = finalCheck(ctx); e != nil {
			return j.fault(e)
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		if !windowCurrent(grant, w) || !recordCurrent(v, w) {
			return j.fault(serviceorigin.ErrExpired)
		}
		return nil
	})
	if err != nil {
		if committed {
			r.faulted.Store(true)
			return "", errors.Join(ErrFaulted, err)
		}
		return "", err
	}
	return result, nil
}

// Need compares an untrusted inventory hint to retained state. It grants no
// authority and never changes floors. A conflict requests the exact proof so
// admission, rather than this hint, can reject it.
func (r *RemoteJournal) Need(ctx context.Context, d RecordDescriptor) (needed bool, err error) {
	if r == nil {
		return false, ErrClosed
	}
	if e := validateDescriptor(d, r.base.cfg.Replica.Tenant, r.base.cfg.Replica.Site, r.cfg.LocalDevice); e != nil {
		return false, e
	}
	err = guarded(ctx, r.cfg.Guard, func() error {
		j := r.base
		j.lock(ctx)
		defer j.mu.Unlock()
		if e := j.usable(); e != nil {
			return e
		}
		if r.faulted.Load() {
			return ErrFaulted
		}
		if _, e := j.current(ctx); e != nil {
			return e
		}
		actor := source{d.Reference.Key.Tenant, d.Reference.Key.Source.Device, d.Reference.Key.Source.App, d.Reference.Key.Source.Service}
		if d.Reference.Version.Epoch < j.epochs[sourceKey(actor)] {
			return nil
		}
		old, exists := j.proofs[componentKey(d.Reference.Key)]
		if !exists {
			needed = true
			return nil
		}
		_, record, e := decodeRemoteProof(old.Envelope)
		if e != nil {
			return j.fault(e)
		}
		order := d.Reference.Version.Compare(world.Version{Epoch: record.Epoch, Sequence: record.Sequence})
		if order < 0 {
			return nil
		}
		if order > 0 || digest(old.Envelope) != d.Reference.EnvelopeSHA256 || digest(rawRecord(old.Envelope)) != d.Reference.RecordSHA256 || record.Publication.Tombstone != d.Tombstone {
			needed = true
			return nil
		}
		checkpoint, e := j.replica.Checkpoint()
		if e != nil {
			return e
		}
		for _, entry := range checkpoint.Entries {
			if entry.Component.Key == d.Reference.Key {
				needed = d.ExpiresAtUnixNS < entry.ExpiresAt.UnixNano()
				return nil
			}
		}
		return j.fault(ErrCorrupt)
	})
	return needed, err
}
