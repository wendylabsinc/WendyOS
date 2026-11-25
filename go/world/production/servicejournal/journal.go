package servicejournal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func (j *Journal) usable() error {
	if j == nil || j.closed {
		return ErrClosed
	}
	if j.faulted {
		return ErrFaulted
	}
	return nil
}
func (j *Journal) fault(err error) error {
	j.observeFault(err)
	j.faulted = true
	return fmt.Errorf("%w: %v", ErrFaulted, err)
}
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.db.Close()
}

func (j *Journal) current(ctx context.Context) (trustedtime.Interval, error) {
	if err := ctx.Err(); err != nil {
		return trustedtime.Interval{}, err
	}
	w, err := j.physicalNow()
	if err != nil {
		j.observeFault(err)
		j.faulted = true
		return trustedtime.Interval{}, err
	}
	return w, nil
}

// ReserveEpoch commits before returning. It does not depend on certificate,
// publication key or deployment generation; gaps after a failed start are safe.
func (j *Journal) ReserveEpoch(ctx context.Context, tenant, device, app, service string) (uint64, error) {
	if j == nil || ctx == nil {
		return 0, ErrEpoch
	}
	j.lock(ctx)
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return 0, err
	}
	s := source{tenant, device, app, service}
	if tenant != j.cfg.Replica.Tenant || validateSource(s, j.cfg.Replica.Site) != nil {
		return 0, ErrEpoch
	}
	key := sourceKey(s)
	previous := j.epochs[key]
	if previous == math.MaxUint64 {
		return 0, ErrEpoch
	}
	if previous == 0 && len(j.epochs) >= j.cfg.MaxEntries {
		return 0, ErrCapacity
	}
	current, err := j.current(ctx)
	if err != nil {
		return 0, err
	}
	checkpoint, err := traceValue(ctx, rpctrace.StageReplica, j.replica.Checkpoint)
	if err != nil {
		return 0, err
	}
	next := previous + 1
	committing := false
	err = j.commitTrace(ctx, func(tx *bolt.Tx) error {
		current, err = j.current(ctx)
		if err != nil {
			return err
		}
		stored := storedEpoch{Epoch: next, SHA256: epochDigest(key, next)}
		value, e := encode(stored)
		if e != nil {
			return e
		}
		if e = tx.Bucket(epochsBucket).Put([]byte(key), value); e != nil {
			return e
		}
		if e = putChecked(tx.Bucket(metaBucket), checkpointKey, checkpoint); e != nil {
			return e
		}
		if e = putChecked(tx.Bucket(metaBucket), floorKey, current); e != nil {
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
			return 0, j.fault(err)
		}
		return 0, err
	}
	j.epochs[key] = next
	j.floor = current
	if _, err = j.current(ctx); err != nil {
		return 0, j.fault(err)
	}
	return next, nil
}

// schema receives a fresh projection so even a misbehaving callback cannot
// alter the checked record subsequently committed or handed to the replica.
func (j *Journal) schema(ctx context.Context, verified serviceorigin.VerifiedRecord) error {
	defer rpctrace.Start(ctx, rpctrace.StageJournalSchema)()
	copy := verified
	copy.Record = serviceorigin.Record{}
	copy.Delegation = serviceorigin.DelegationClaims{}
	if jws.DecodeStrict(verified.Bytes, &copy.Record) != nil {
		return ErrCorrupt
	}
	copy.Bytes = bytes.Clone(verified.Bytes)
	claims, err := encode(verified.Delegation)
	if err != nil {
		return err
	}
	if jws.DecodeStrict(claims, &copy.Delegation) != nil {
		return ErrCorrupt
	}
	if err = j.cfg.AdmitSchema(ctx, copy); err != nil {
		return err
	}
	return ctx.Err()
}

func (j *Journal) verify(ctx context.Context, signed serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error) {
	verifyDone := rpctrace.Start(ctx, rpctrace.StageVerify)
	verified, err := j.cfg.Verifier.Verify(ctx, signed)
	verifyDone()
	if err != nil {
		return verified, err
	}
	if verified.Record.Scope.Tenant != j.cfg.Replica.Tenant || verified.Record.Scope.Site != j.cfg.Replica.Site {
		return verified, serviceorigin.ErrDenied
	}
	if err = j.schema(ctx, verified); err != nil {
		return verified, err
	}
	return verified, nil
}

// Admit accepts only the source epoch this local journal reserved. It verifies
// before staging and again inside the serialized transaction. The commit holds
// checkpoint and original proof together. An error after commit starts is an
// uncertain outcome and faults the journal until reopen.
func (j *Journal) Admit(ctx context.Context, signed serviceorigin.SignedRecord) (world.Result, error) {
	if j == nil || ctx == nil {
		return "", ErrClosed
	}
	j.lock(ctx)
	usable := j.usable()
	j.mu.Unlock()
	if usable != nil {
		return "", usable
	}
	if !envelopeBounds(signed) {
		return "", ErrCapacity
	}
	// Capture one owned envelope before waiting for any authorization or lock.
	raw, err := encode(signed)
	if err != nil || len(raw) > maxEnvelopeBytes {
		return "", ErrCapacity
	}
	signed, _, err = decodeProof(raw)
	if err != nil {
		return "", err
	}
	if _, err = j.verify(ctx, signed); err != nil {
		return "", err
	}
	j.lock(ctx)
	defer j.mu.Unlock()
	if err = j.usable(); err != nil {
		return "", err
	}
	if _, err = j.current(ctx); err != nil {
		return "", err
	}
	verified, err := j.verify(ctx, signed)
	if err != nil {
		return "", err
	}
	if j.epochs[sourceKey(fromScope(verified.Record.Scope))] != verified.Record.Epoch {
		return "", ErrEpoch
	}
	component := verified.Component()
	key := componentKey(component.Key)
	old, exists := j.proofs[key]
	if exists {
		_, record, e := decodeProof(old.Envelope)
		if e != nil {
			return "", j.fault(e)
		}
		if record.Epoch == verified.Record.Epoch && record.Sequence == verified.Record.Sequence && !bytes.Equal(old.Envelope, raw) {
			return "", world.ErrConflict
		}
	}
	candidate, err := traceValue(ctx, rpctrace.StageReplica, j.replica.Clone)
	if err != nil {
		return "", err
	}
	result, err := traceValue(ctx, rpctrace.StageReplica, func() (world.Result, error) { return candidate.Apply(component.Key.Source, component) })
	if err != nil {
		return "", err
	}
	proof := storedProof{SHA256: digest(raw), Envelope: raw}
	total := j.proofBytes
	if result == world.Applied {
		if !exists && len(j.proofs) >= j.cfg.MaxEntries {
			return "", ErrCapacity
		}
		total += int64(len(raw)) - int64(len(old.Envelope))
		if total > j.cfg.MaxProofBytes {
			return "", ErrCapacity
		}
	}
	checkpoint, err := traceValue(ctx, rpctrace.StageReplica, candidate.Checkpoint)
	if err != nil {
		return "", err
	}
	checkpointRaw, err := encode(checkpoint)
	if err != nil || int64(len(checkpointRaw)) > j.cfg.MaxProofBytes {
		return "", ErrCapacity
	}
	var floor trustedtime.Interval
	committing := false
	err = j.commitTrace(ctx, func(tx *bolt.Tx) error {
		if _, e := j.verify(ctx, signed); e != nil {
			return e
		}
		var e error
		floor, e = j.current(ctx)
		if e != nil {
			return e
		}
		if result == world.Applied {
			value, e := encode(proof)
			if e != nil {
				return e
			}
			if e = tx.Bucket(proofsBucket).Put([]byte(key), value); e != nil {
				return e
			}
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
	if err != nil {
		if committing {
			return "", j.fault(err)
		}
		return "", err
	}
	j.replica = candidate
	j.floor = floor
	if result == world.Applied {
		j.proofs[key] = proof
		j.proofBytes = total
	}
	if _, err = j.verify(ctx, signed); err != nil {
		return "", j.fault(err)
	}
	if _, err = j.current(ctx); err != nil {
		return "", j.fault(err)
	}
	return result, nil
}

// Snapshot returns only records live in the original source clock and still
// authorized now. Expired/revoked proofs remain historical high-water entries.
// Returned proofs are unchanged; this method never extends their lifetime.
func (j *Journal) Snapshot(ctx context.Context) ([]serviceorigin.SignedRecord, error) {
	return j.snapshot(ctx, nil)
}

// SnapshotSelected performs the same current proof, schema and final lifetime
// checks as Snapshot, but only for these exact source selectors. Selection is
// applied before proof decoding or schema/media work. Selectors are filters,
// not grants; the host must still authorize every requested source. An empty,
// duplicate, foreign-site or invalid selection is never a wildcard.
func (j *Journal) SnapshotSelected(ctx context.Context, sources []proofref.Source) ([]serviceorigin.SignedRecord, error) {
	if j == nil || ctx == nil {
		return nil, ErrClosed
	}
	if len(sources) == 0 || len(sources) > 64 {
		return nil, ErrCapacity
	}
	selected := make(map[proofref.Source]struct{}, len(sources))
	for _, source := range sources {
		if source.Validate() != nil || source.Tenant != j.cfg.Replica.Tenant || source.Site != j.cfg.Replica.Site {
			return nil, serviceorigin.ErrDenied
		}
		if _, duplicate := selected[source]; duplicate {
			return nil, serviceorigin.ErrDenied
		}
		selected[source] = struct{}{}
	}
	return j.snapshot(ctx, selected)
}

func (j *Journal) snapshot(ctx context.Context, selected map[proofref.Source]struct{}) ([]serviceorigin.SignedRecord, error) {
	defer rpctrace.Start(ctx, rpctrace.StageJournalSnapshot)()
	if j == nil || ctx == nil {
		return nil, ErrClosed
	}
	j.lock(ctx)
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, err
	}
	if _, err := j.current(ctx); err != nil {
		return nil, err
	}
	live, err := traceValue(ctx, rpctrace.StageReplica, j.replica.Snapshot)
	if err != nil {
		return nil, err
	}
	result := make([]serviceorigin.SignedRecord, 0, len(live))
	for _, component := range live {
		if selected != nil {
			if _, ok := selected[(proofref.Reference{Key: component.Key}).Source()]; !ok {
				continue
			}
		}
		stored, ok := j.proofs[componentKey(component.Key)]
		if !ok {
			return nil, j.fault(ErrCorrupt)
		}
		signed, _, e := decodeProof(stored.Envelope)
		if e != nil {
			return nil, j.fault(e)
		}
		if _, e = j.verify(ctx, signed); e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(e, serviceorigin.ErrClock) {
				return nil, j.fault(e)
			}
			continue
		}
		result = append(result, signed)
	}
	checkpoint, err := traceValue(ctx, rpctrace.StageReplica, j.replica.Checkpoint)
	if err != nil {
		return nil, err
	}
	var floor trustedtime.Interval
	committing := false
	err = j.commitTrace(ctx, func(tx *bolt.Tx) error {
		var e error
		floor, e = j.current(ctx)
		if e != nil {
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
	if err != nil {
		if committing {
			return nil, j.fault(err)
		}
		return nil, err
	}
	j.floor = floor
	type snapshotCandidate struct {
		proof       serviceorigin.SignedRecord
		key         string
		from, until int64
	}
	filtered := make([]snapshotCandidate, 0, len(result))
	for _, signed := range result {
		verified, e := j.verify(ctx, signed)
		if e != nil {
			if ctx.Err() != nil {
				return nil, j.fault(ctx.Err())
			}
			if errors.Is(e, serviceorigin.ErrClock) {
				return nil, j.fault(e)
			}
			continue
		}
		filtered = append(filtered, snapshotCandidate{proof: signed, key: componentKey(verified.Component().Key), from: verified.Record.IssuedAtUnixMS, until: verified.Record.ExpiresAtUnixMS})
	}
	// Later records may have delayed the earlier checks. Use one final source
	// snapshot and physical interval for the entire returned group.
	liveNow, err := traceValue(ctx, rpctrace.StageReplica, j.replica.Snapshot)
	if err != nil {
		return nil, j.fault(err)
	}
	visible := make(map[string]bool, len(liveNow))
	for _, component := range liveNow {
		visible[componentKey(component.Key)] = true
	}
	final, err := j.current(ctx)
	if err != nil {
		return nil, j.fault(err)
	}
	result = result[:0]
	for _, candidate := range filtered {
		if visible[candidate.key] && !final.Earliest.Before(time.UnixMilli(candidate.from)) && final.Latest.Before(time.UnixMilli(candidate.until)) {
			result = append(result, candidate.proof)
		}
	}
	return result, nil
}

func envelopeBounds(s serviceorigin.SignedRecord) bool {
	if len(s.Record) == 0 || len(s.Record) > serviceorigin.MaxRecordBytes || len(s.Signature) != ed25519.SignatureSize || len(s.Delegation.Claims) == 0 || len(s.Delegation.Claims) > serviceorigin.MaxClaimsBytes || len(s.Delegation.Signature) != mldsa.MLDSA65SignatureSize || len(s.Delegation.ChainDER) == 0 || len(s.Delegation.ChainDER) > 8 {
		return false
	}
	total := 0
	for _, der := range s.Delegation.ChainDER {
		if len(der) == 0 || len(der) > (64<<10)-total {
			return false
		}
		total += len(der)
	}
	return true
}
