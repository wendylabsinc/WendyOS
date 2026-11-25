package proofarchive

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func (a *Archive) usable() error {
	if a == nil || a.closed {
		return ErrClosed
	}
	if a.fault != nil {
		return errors.Join(ErrFaulted, a.fault)
	}
	return nil
}
func (a *Archive) fail(err error) error { a.fault = err; return err }

// A refusal can itself observe expiry. Persist that known physical floor even
// when the data transaction rolls back, so reopening cannot revive the proof.
func (a *Archive) finishFailure(ctx context.Context, original error) error {
	if a.closed || errors.Is(a.fault, ErrUncertain) || errors.Is(a.fault, ErrCorrupt) {
		return original
	}
	if a.fault == nil {
		_, _ = a.now(context.Background())
	}
	if a.observed.Validate() != nil {
		return original
	}
	w := a.observed
	committing := false
	err := a.commitTrace(ctx, func(tx *bolt.Tx) error {
		if e := put(tx.Bucket(metaBucket), floorKey, w); e != nil {
			return e
		}
		committing = true
		return nil
	})
	if err != nil {
		if committing {
			return a.fail(errors.Join(original, ErrUncertain, err))
		}
		return a.fail(errors.Join(original, ErrFaulted, err))
	}
	a.floor = w
	return original
}
func (a *Archive) now(ctx context.Context) (trustedtime.Interval, error) {
	if ctx == nil {
		return trustedtime.Interval{}, ErrDenied
	}
	if e := ctx.Err(); e != nil {
		return trustedtime.Interval{}, e
	}
	if e := a.usable(); e != nil {
		return trustedtime.Interval{}, e
	}
	w, e := a.cfg.Clock.Now()
	if e != nil || w.Validate() != nil {
		return w, a.fail(ErrClock)
	}
	for i, old := range []trustedtime.Interval{a.floor, a.observed} {
		if old.Earliest.IsZero() {
			continue
		}
		if w.Earliest.Before(old.Earliest) || w.Latest.Before(old.Latest) || w.BootID == old.BootID && w.Monotonic < old.Monotonic || i == 1 && w.BootID != old.BootID {
			return w, a.fail(ErrClock)
		}
	}
	a.observed = w
	return w, nil
}
func (a *Archive) transaction(ctx context.Context, work func(*bolt.Tx, trustedtime.Interval) error) (trustedtime.Interval, error) {
	if _, e := a.now(ctx); e != nil {
		return trustedtime.Interval{}, e
	}
	var floor trustedtime.Interval
	committing := false
	err := a.commitTrace(ctx, func(tx *bolt.Tx) error {
		w, e := a.now(ctx)
		if e != nil {
			return e
		}
		if e = work(tx, w); e != nil {
			return e
		}
		floor, e = a.now(ctx)
		if e != nil {
			return e
		}
		u, e := a.usageTrace(ctx, tx)
		if e != nil {
			return e
		}
		if e = put(tx.Bucket(metaBucket), usageKey, u); e != nil {
			return e
		}
		if e = put(tx.Bucket(metaBucket), floorKey, floor); e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		committing = true
		return nil
	})
	if err != nil {
		if committing {
			return trustedtime.Interval{}, a.fail(errors.Join(ErrUncertain, err))
		}
		if errors.Is(err, ErrCorrupt) {
			a.fail(err)
		}
		return trustedtime.Interval{}, err
	}
	a.floor = floor
	after, err := a.now(ctx)
	if err != nil {
		return trustedtime.Interval{}, a.fail(errors.Join(ErrUncertain, err))
	}
	return after, nil
}
func (a *Archive) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return a.db.Close()
}
func (a *Archive) verify(ctx context.Context, raw []byte) (serviceorigin.VerifiedRecord, Reference, error) {
	return a.verifyFor(ctx, raw, false)
}
func (a *Archive) verifyFor(ctx context.Context, raw []byte, remote bool) (serviceorigin.VerifiedRecord, Reference, error) {
	defer rpctrace.Start(ctx, rpctrace.StageArchiveVerify)()
	signed, ref, err := a.decodeEnvelope(raw)
	if err != nil {
		return serviceorigin.VerifiedRecord{}, ref, err
	}
	verifier := a.cfg.Verifier
	if remote {
		if a.localScope(ref) || a.cfg.RemoteVerifier == nil || a.cfg.ValidateRemoteAccepted == nil {
			return serviceorigin.VerifiedRecord{}, ref, ErrDenied
		}
		verifier = a.cfg.RemoteVerifier
	} else if !a.localScope(ref) {
		return serviceorigin.VerifiedRecord{}, ref, ErrDenied
	}
	v, err := verifier.Verify(ctx, signed)
	if err != nil {
		if errors.Is(err, serviceorigin.ErrClock) {
			a.fail(ErrClock)
		}
		return v, ref, err
	}
	if !bytes.Equal(v.Bytes, signed.Record) || v.Record.Scope.Tenant != a.cfg.Tenant || v.Record.Scope.Site != a.cfg.Site || v.Record.Scope.Device != ref.Key.Source.Device {
		return v, ref, ErrDenied
	}
	if remote {
		if err = a.cfg.ValidateRemoteAccepted(ctx, v, bytes.Clone(raw)); err != nil {
			return serviceorigin.VerifiedRecord{}, ref, err
		}
		if err = ctx.Err(); err != nil {
			return serviceorigin.VerifiedRecord{}, ref, err
		}
	}
	return v, ref, nil
}

// RememberAccepted is only for the host AFTER successful local journal admission
// and BEFORE acknowledging the app. A later archive error does not roll back the
// journal; the host must report the publication's uncertain overall outcome.
func (a *Archive) RememberAccepted(ctx context.Context, envelope []byte) (result Reference, err error) {
	return a.rememberAccepted(ctx, envelope, false)
}
func (a *Archive) rememberAccepted(ctx context.Context, envelope []byte, remote bool) (result Reference, err error) {
	defer rpctrace.Start(ctx, rpctrace.StageArchiveRemember)()
	if a == nil {
		return Reference{}, ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil {
			err = a.finishFailure(ctx, err)
		}
	}()
	if _, e := a.now(ctx); e != nil {
		return Reference{}, e
	}
	if int64(len(envelope)) > a.cfg.MaxEnvelopeBytes {
		return Reference{}, ErrQuota
	}
	owned := bytes.Clone(envelope)
	_, ref, err := a.verifyFor(ctx, owned, remote)
	if err != nil {
		return Reference{}, err
	}
	var retainedUntil int64
	after, err := a.transaction(ctx, func(tx *bolt.Tx, w trustedtime.Interval) error {
		if _, _, e := a.verifyFor(ctx, owned, remote); e != nil {
			return e
		}
		if old, e := a.readProof(tx, ref); e == nil {
			if !bytes.Equal(old.Envelope, owned) {
				return ErrConflict
			}
			if old.RetainUntilUnixMS <= w.Latest.UnixMilli() {
				return ErrUnavailable
			}
			retainedUntil = old.RetainUntilUnixMS
			return nil
		} else if !errors.Is(e, ErrUnavailable) {
			return e
		}
		h, exists, e := water(tx, rememberBucket, ref.Key)
		if e != nil {
			return e
		}
		if exists && ref.Version.Compare(h.Version) <= 0 {
			if ref.Version == h.Version && ref != h {
				return ErrConflict
			}
			return ErrUnavailable
		}
		current, e := a.now(ctx)
		if e != nil {
			return e
		}
		recent := current.Earliest.Add(a.cfg.RecentWindow).UnixMilli()
		if recent <= current.Latest.UnixMilli() {
			return ErrUnavailable
		}
		d := Descriptor{Reference: ref, Envelope: owned, Admitted: current, RecentUntilUnixMS: recent, RetainUntilUnixMS: recent}
		if pin, e := a.readPin(tx, ref); e == nil {
			if pin.RetainUntilUnixMS <= current.Latest.UnixMilli() {
				return ErrUnavailable
			}
			d.RetainUntilUnixMS = max(d.RetainUntilUnixMS, pin.RetainUntilUnixMS)
		} else if !errors.Is(e, ErrUnavailable) {
			return e
		}
		if e = put(tx.Bucket(proofBucket), []byte(ref.EnvelopeSHA256), d); e != nil {
			return e
		}
		retainedUntil = d.RetainUntilUnixMS
		return put(tx.Bucket(rememberBucket), []byte(key(ref.Key)), ref)
	})
	if err != nil {
		return Reference{}, err
	}
	if retainedUntil <= after.Latest.UnixMilli() {
		return Reference{}, a.fail(errors.Join(ErrUncertain, ErrUnavailable))
	}
	if _, _, err = a.verifyFor(ctx, owned, remote); err != nil {
		return Reference{}, a.fail(errors.Join(ErrUncertain, err))
	}
	if after, err = a.now(ctx); err != nil {
		return Reference{}, a.fail(errors.Join(ErrUncertain, err))
	}
	if retainedUntil <= after.Latest.UnixMilli() {
		return Reference{}, a.fail(errors.Join(ErrUncertain, ErrUnavailable))
	}
	return ref, nil
}

func cloneDescriptors(in []Descriptor) []Descriptor {
	out := append([]Descriptor(nil), in...)
	for i := range out {
		out[i].Envelope = bytes.Clone(out[i].Envelope)
	}
	return out
}
func receipt(p storedPin) PinReceipt {
	return PinReceipt{Claim: p.Claim, Inputs: append([]Reference(nil), p.Inputs...), RetainUntilUnixMS: p.RetainUntilUnixMS}
}
func (a *Archive) inputs(tx *bolt.Tx, refs []Reference, w trustedtime.Interval, until int64, recent bool) ([]Descriptor, error) {
	out := make([]Descriptor, 0, len(refs))
	for _, r := range refs {
		d, e := a.readProof(tx, r)
		if e != nil {
			return nil, e
		}
		if d.RetainUntilUnixMS <= w.Latest.UnixMilli() || recent && d.RecentUntilUnixMS <= w.Latest.UnixMilli() || until > d.Admitted.Earliest.Add(a.cfg.MaxRetention).UnixMilli() {
			return nil, ErrUnavailable
		}
		out = append(out, d)
	}
	return out, nil
}
func (a *Archive) validatePin(ctx context.Context, raw []byte, inputs []Descriptor, until int64) error {
	verified, _, e := a.verify(ctx, raw)
	if e != nil {
		return e
	}
	// Callbacks receive independent envelope bytes and cannot mutate storage.
	if e = a.cfg.ValidatePin(ctx, verified, cloneDescriptors(inputs), time.UnixMilli(until)); e != nil {
		return e
	}
	return ctx.Err()
}

// Pin atomically preserves every required input or none. Its immutable claim
// identity is the exact signed envelope; failed later journal admission may
// leave a bounded orphan pin, which is never rolled back on an uncertain result.
func (a *Archive) Pin(ctx context.Context, request PinRequest) (result PinReceipt, err error) {
	if a == nil {
		return PinReceipt{}, ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil {
			err = a.finishFailure(ctx, err)
		}
	}()
	w, e := a.now(ctx)
	if e != nil {
		return PinReceipt{}, e
	}
	if int64(len(request.ClaimEnvelope)) > a.cfg.MaxEnvelopeBytes || len(request.Inputs) == 0 || len(request.Inputs) > a.cfg.MaxRefsPerPin {
		return PinReceipt{}, ErrQuota
	}
	raw := bytes.Clone(request.ClaimEnvelope)
	refs := append([]Reference(nil), request.Inputs...)
	_, claim, e := a.verify(ctx, raw)
	if e != nil {
		return PinReceipt{}, e
	}
	seen := map[Reference]bool{}
	for _, r := range refs {
		if !a.inputScope(r) || r == claim || seen[r] {
			return PinReceipt{}, ErrDenied
		}
		seen[r] = true
	}
	until := request.RetainUntilUnixMS
	if until <= w.Latest.UnixMilli() || until > w.Earliest.Add(a.cfg.MaxRetention).UnixMilli() {
		return PinReceipt{}, ErrDenied
	}
	var existing bool
	var descriptors []Descriptor
	var out storedPin
	e = a.db.View(func(tx *bolt.Tx) error {
		if p, e := a.readPin(tx, claim); e == nil {
			if p.RetainUntilUnixMS != until || !reflect.DeepEqual(p.Inputs, refs) {
				return ErrConflict
			}
			if until <= w.Latest.UnixMilli() {
				return ErrUnavailable
			}
			existing = true
			out = p
		} else if !errors.Is(e, ErrUnavailable) {
			return e
		}
		if !existing {
			h, ok, e := water(tx, pinWaterBucket, claim.Key)
			if e != nil {
				return e
			}
			if ok && claim.Version.Compare(h.Version) <= 0 {
				if claim.Version == h.Version && claim != h {
					return ErrConflict
				}
				return ErrUnavailable
			}
		}
		// A late pin may extend a still-retained claim, but never revive one
		// whose archive lifetime already ended, whether pruned or not.
		if own, e := a.readProof(tx, claim); e == nil {
			if own.RetainUntilUnixMS <= w.Latest.UnixMilli() {
				return ErrUnavailable
			}
		} else if errors.Is(e, ErrUnavailable) {
			h, ok, e := water(tx, rememberBucket, claim.Key)
			if e != nil {
				return e
			}
			if ok && claim.Version.Compare(h.Version) <= 0 {
				return ErrUnavailable
			}
		} else {
			return e
		}
		var e error
		descriptors, e = a.inputs(tx, refs, w, until, !existing)
		return e
	})
	if e != nil {
		if errors.Is(e, ErrCorrupt) {
			a.fail(e)
		}
		return PinReceipt{}, e
	}
	// Mandatory all-input authority/schema check before opening the write tx.
	if e = a.validatePin(ctx, raw, descriptors, until); e != nil {
		return PinReceipt{}, e
	}
	after, e := a.transaction(ctx, func(tx *bolt.Tx, current trustedtime.Interval) error {
		var e error
		descriptors, e = a.inputs(tx, refs, current, until, !existing)
		if e != nil {
			return e
		}
		if e = a.validatePin(ctx, raw, descriptors, until); e != nil {
			return e
		}
		current, e = a.now(ctx)
		if e != nil {
			return e
		}
		// Recheck every input after callbacks, whose clock/policy I/O may wait.
		descriptors, e = a.inputs(tx, refs, current, until, !existing)
		if e != nil {
			return e
		}
		if until <= current.Latest.UnixMilli() {
			return ErrUnavailable
		}
		if existing {
			return nil
		}
		out = storedPin{Claim: claim, Inputs: refs, RetainUntilUnixMS: until, Created: current}
		for _, d := range descriptors {
			d.RetainUntilUnixMS = max(d.RetainUntilUnixMS, until)
			if e = put(tx.Bucket(proofBucket), []byte(d.Reference.EnvelopeSHA256), d); e != nil {
				return e
			}
		}
		// A caller may already have archived this claim. Retention still binds
		// to exactly the same bytes; normal integration pins before admission.
		if own, e := a.readProof(tx, claim); e == nil {
			if own.RetainUntilUnixMS <= current.Latest.UnixMilli() {
				return ErrUnavailable
			}
			own.RetainUntilUnixMS = max(own.RetainUntilUnixMS, until)
			if own.RetainUntilUnixMS > own.Admitted.Earliest.Add(a.cfg.MaxRetention).UnixMilli() {
				return ErrDenied
			}
			if e = put(tx.Bucket(proofBucket), []byte(claim.EnvelopeSHA256), own); e != nil {
				return e
			}
		} else if !errors.Is(e, ErrUnavailable) {
			return e
		}
		if e = put(tx.Bucket(pinBucket), []byte(claim.EnvelopeSHA256), out); e != nil {
			return e
		}
		return put(tx.Bucket(pinWaterBucket), []byte(key(claim.Key)), claim)
	})
	if e != nil {
		return PinReceipt{}, e
	}
	if until <= after.Latest.UnixMilli() {
		return PinReceipt{}, a.fail(errors.Join(ErrUncertain, ErrUnavailable))
	}
	if e = a.validatePin(ctx, raw, descriptors, until); e != nil {
		return PinReceipt{}, a.fail(errors.Join(ErrUncertain, e))
	}
	after, e = a.now(ctx)
	if e != nil || until <= after.Latest.UnixMilli() {
		return PinReceipt{}, a.fail(errors.Join(ErrUncertain, e, ErrUnavailable))
	}
	return receipt(out), nil
}

// Get requires an exact source chosen by the host's current reader grants and
// lifecycle fence. Hash possession never chooses that source. This method does
// not invoke the current publication verifier or return a live component.
func (a *Archive) Get(ctx context.Context, expected Source, ref Reference) (result HistoricalProof, err error) {
	if a == nil {
		return HistoricalProof{}, ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil {
			err = a.finishFailure(ctx, err)
		}
	}()
	if expected.Validate() != nil || !a.scope(ref) || expected != ref.Source() {
		return HistoricalProof{}, ErrUnavailable
	}
	var d Descriptor
	after, e := a.transaction(ctx, func(tx *bolt.Tx, w trustedtime.Interval) error {
		var e error
		d, e = a.readProof(tx, ref)
		if e != nil {
			return e
		}
		if d.RetainUntilUnixMS <= w.Latest.UnixMilli() {
			return ErrUnavailable
		}
		return nil
	})
	if e != nil {
		return HistoricalProof{}, e
	}
	if d.RetainUntilUnixMS <= after.Latest.UnixMilli() {
		return HistoricalProof{}, ErrUnavailable
	}
	return HistoricalProof{Descriptor: d, Historical: true}, nil
}

// CheckPin confirms storage retention only. The host separately checks the
// claim's current authority and original input validity before showing it live.
func (a *Archive) CheckPin(ctx context.Context, claim Reference) (result PinReceipt, err error) {
	if a == nil {
		return PinReceipt{}, ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil {
			err = a.finishFailure(ctx, err)
		}
	}()
	if !a.localScope(claim) {
		return PinReceipt{}, ErrUnavailable
	}
	var p storedPin
	after, e := a.transaction(ctx, func(tx *bolt.Tx, w trustedtime.Interval) error {
		var e error
		p, e = a.readPin(tx, claim)
		if e != nil {
			return e
		}
		if p.RetainUntilUnixMS <= w.Latest.UnixMilli() {
			return ErrUnavailable
		}
		for _, ref := range p.Inputs {
			d, e := a.readProof(tx, ref)
			if e != nil {
				return e
			}
			if d.RetainUntilUnixMS < p.RetainUntilUnixMS {
				return ErrCorrupt
			}
		}
		return nil
	})
	if e != nil {
		return PinReceipt{}, e
	}
	if p.RetainUntilUnixMS <= after.Latest.UnixMilli() {
		return PinReceipt{}, ErrUnavailable
	}
	return receipt(p), nil
}
func (a *Archive) Prune(ctx context.Context) (err error) {
	defer rpctrace.Start(ctx, rpctrace.StageArchivePrune)()
	if a == nil {
		return ErrClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer func() {
		if err != nil {
			err = a.finishFailure(ctx, err)
		}
	}()
	_, e := a.transaction(ctx, func(tx *bolt.Tx, w trustedtime.Interval) error {
		var remove [][]byte
		if e := tx.Bucket(pinBucket).ForEach(func(k, v []byte) error {
			var p storedPin
			if unwrap(v, &p, 128<<10) != nil {
				return ErrCorrupt
			}
			p, e := a.readPin(tx, p.Claim)
			if e != nil {
				return e
			}
			if p.RetainUntilUnixMS <= w.Latest.UnixMilli() {
				remove = append(remove, bytes.Clone(k))
			}
			return nil
		}); e != nil {
			return e
		}
		for _, k := range remove {
			if e := tx.Bucket(pinBucket).Delete(k); e != nil {
				return e
			}
		}
		remove = nil
		if e := tx.Bucket(proofBucket).ForEach(func(k, v []byte) error {
			var d Descriptor
			if unwrap(v, &d, a.cfg.MaxEnvelopeBytes*2+16<<10) != nil {
				return ErrCorrupt
			}
			d, e := a.readProof(tx, d.Reference)
			if e != nil {
				return e
			}
			if d.RetainUntilUnixMS <= w.Latest.UnixMilli() {
				remove = append(remove, bytes.Clone(k))
			}
			return nil
		}); e != nil {
			return e
		}
		for _, k := range remove {
			if e := tx.Bucket(proofBucket).Delete(k); e != nil {
				return e
			}
		}
		return nil
	})
	return e
}
