package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ErrRecordClockRollback is a verifier health failure, not an origin denial
// that repair may omit while continuing with other records.
var ErrRecordClockRollback = errors.New("record verification clock regressed")

const (
	MaxRecordBatchSize  = 8
	MaxRecordBatchBytes = 512 << 10
)

// VerifyRecords authenticates a bounded group of immutable origin proofs. It
// returns identities in input order only when the entire group is authorized.
// Records do not consume HTTP nonces; replica versions handle their duplicates.
//
// Every record must pass signature, issuance-grant and current revocation checks
// before any replay write. Unlike sequential VerifyRecord calls, a forged or
// identity-invalid member therefore cannot leave a partially admitted group.
// Authenticated late binding denials retain one original clock/policy checkpoint,
// matching the standalone record check's durable observation at that boundary.
//
// The final check follows all replay I/O and can only narrow the admitted
// authority. Its later clock sample is not a new persisted floor. Issuance-grant
// expiry does not invalidate an otherwise current repair proof. This method
// does not change VerifyRecord's existing standalone behavior. Returned
// identities describe this verification point, not authority reserved across
// subsequent node/world persistence or network operations.
func (v *Verifier) VerifyRecords(records []SignedRecord) ([]Identity, error) {
	return v.VerifyRecordsContext(context.Background(), records)
}
func (v *Verifier) VerifyRecordsContext(ctx context.Context, records []SignedRecord) ([]Identity, error) {
	if len(records) == 0 || len(records) > MaxRecordBatchSize {
		return nil, ErrDenied
	}
	encoded, err := json.Marshal(records)
	if err != nil || len(encoded) > MaxRecordBatchBytes {
		return nil, ErrDenied
	}
	// Preserve the original values across blocking calls. A JSON round trip
	// would compact raw payloads before the existing byte limit and could
	// normalize distinct invalid-UTF-8 source strings into the same identity.
	group := copyRecordBatch(records)
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	now := v.cfg.Now()
	operations := make([]string, len(group))
	var checkpoint identityCheckpoint
	var lateDenial error
	for index, record := range group {
		if record.Version != Version || !validWindow(record.SignedAtUnixMS, record.ExpiresAtUnixMS, v.cfg.MaxRecordLifetime) || now.UnixMilli() < record.SignedAtUnixMS || now.UnixMilli() >= record.ExpiresAtUnixMS || !verify(record.Grant.Grant.PublicKey, "record", recordPayload(record), record.Signature) {
			return nil, ErrDenied
		}
		operation := "publish"
		if annotations.Reserved(record.Component) {
			if err = validateAnnotationRecord(record); err != nil {
				return nil, err
			}
			operation = "annotation-write"
			if err := v.annotationAmendmentProof(record); err != nil {
				return nil, err
			}
			if err := v.annotationClockRecord(record, now, false); err != nil {
				return nil, err
			}
		}
		id, admitted, err := v.validateIdentity(record.Grant, operation, time.UnixMilli(record.SignedAtUnixMS), now)
		if err != nil {
			return nil, err
		}
		// v.mu prevents the installed policy from changing within the group.
		checkpoint = admitted
		operations[index] = operation
		component := record.Component
		if component.Key.Source != id.Source || component.Key.Tenant != v.cfg.Tenant || component.Key.Site != v.cfg.Site || len(component.Payload) > v.cfg.MaxBodyBytes || (v.cfg.Domain == "physical" && component.Provenance.Synthetic()) {
			lateDenial = ErrDenied
		}
	}
	if err = replayCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return nil, err
	}
	if lateDenial != nil {
		return nil, lateDenial
	}
	// This read may wait for another ledger operation. Perform it before the
	// final sample, and conservatively deny if a different writer advanced the
	// floor past the original admission time. Never retry a failed checkpoint.
	if err = replayValidateCheckpoint(ctx, v.cfg.Replay, checkpoint.revision, checkpoint.digest, now); err != nil {
		return nil, err
	}
	completed := v.cfg.Now()
	if completed.Round(0).Before(now.Round(0)) {
		return nil, ErrRecordClockRollback
	}
	identities := make([]Identity, len(group))
	for index, record := range group {
		if completed.UnixMilli() < record.SignedAtUnixMS || completed.UnixMilli() >= record.ExpiresAtUnixMS {
			return nil, ErrDenied
		}
		id, _, err := v.validateIdentity(record.Grant, operations[index], time.UnixMilli(record.SignedAtUnixMS), completed)
		if err != nil {
			return nil, err
		}
		if annotations.Reserved(record.Component) {
			if err := v.annotationClockRecord(record, completed, false); err != nil {
				return nil, err
			}
		}
		id.NotAfter = time.UnixMilli(record.ExpiresAtUnixMS)
		if annotations.Reserved(record.Component) {
			id.NotAfter = v.annotationDeadline(record)
		}
		identities[index] = id
	}
	return identities, nil
}

func copyRecordBatch(records []SignedRecord) []SignedRecord {
	group := make([]SignedRecord, len(records))
	for index, record := range records {
		record.Signature = bytes.Clone(record.Signature)
		record.Grant.Signature = bytes.Clone(record.Grant.Signature)
		record.Grant.Grant.PublicKey = bytes.Clone(record.Grant.Grant.PublicKey)
		record.Grant.Grant.Scopes = append([]string(nil), record.Grant.Grant.Scopes...)
		record.Component.Payload = bytes.Clone(record.Component.Payload)
		provenance := record.Component.Provenance
		provenance.Simulation = copyBatchSimulation(provenance.Simulation)
		provenance.OriginalCaptureTime = copyBatchTime(provenance.OriginalCaptureTime)
		if provenance.Inputs != nil {
			provenance.Inputs = append([]world.Input{}, provenance.Inputs...)
			for i := range provenance.Inputs {
				provenance.Inputs[i].Simulation = copyBatchSimulation(provenance.Inputs[i].Simulation)
				provenance.Inputs[i].OriginalCaptureTime = copyBatchTime(provenance.Inputs[i].OriginalCaptureTime)
			}
		}
		record.Component.Provenance = provenance
		group[index] = record
	}
	return group
}

func copyBatchSimulation(value *world.SimulationContext) *world.SimulationContext {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyBatchTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
