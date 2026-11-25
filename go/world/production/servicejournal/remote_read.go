package servicejournal

import (
	"bytes"
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

// persistRead keeps source and authenticated physical floors across restart.
// The caller holds its host guard and journal mutex. The returned boolean also
// covers an uncertain synchronous commit, so no later error can imply rollback.
func (j *Journal) persistRead(ctx context.Context, checkpoint world.Checkpoint) (committed bool, err error) {
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
			return true, j.fault(err)
		}
		return false, err
	}
	j.floor = floor
	return true, nil
}

type remoteVisible struct {
	value    RecordExport
	verified serviceorigin.VerifiedRecord
	window   AuthorityWindow
}

// Snapshot returns exact envelopes only while their original samples and
// signing authority, current remote source lease and receiver policy are live.
// Historical reopening never requires expired proofs to pretend to be current.
// The host guard covers the complete returned group and all policy mutations.
func (r *RemoteJournal) Snapshot(ctx context.Context) (out []RecordExport, err error) {
	if r == nil {
		return nil, ErrClosed
	}
	if r.faulted.Load() {
		return nil, ErrFaulted
	}
	committed := false
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
		live, e := j.replica.Snapshot()
		if e != nil {
			return j.fault(e)
		}
		checkpoint, e := j.replica.Checkpoint()
		if e != nil {
			return j.fault(e)
		}
		deadlines := make(map[world.Key]world.CheckpointEntry, len(checkpoint.Entries))
		for _, entry := range checkpoint.Entries {
			deadlines[entry.Component.Key] = entry
		}
		candidates := make([]remoteVisible, 0, len(live))
		for _, component := range live {
			rpctrace.Add(ctx, rpctrace.StageRemoteCandidate, 0)
			stored, ok := j.proofs[componentKey(component.Key)]
			if !ok {
				return j.fault(ErrCorrupt)
			}
			_, record, e := decodeRemoteProof(stored.Envelope)
			if e != nil {
				return j.fault(e)
			}
			if record.Epoch != j.epochs[sourceKey(fromScope(record.Scope))] {
				rpctrace.Add(ctx, rpctrace.StageRemoteEpochOmitted, 0)
				continue
			}
			v, e := r.verify(ctx, stored.Envelope)
			if e != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(e, serviceorigin.ErrClock) {
					return j.fault(e)
				}
				rpctrace.Add(ctx, rpctrace.StageRemoteVerifyOmitted, 0)
				continue
			}
			w, e := j.current(ctx)
			if e != nil {
				return e
			}
			grant, e := r.authorize(ctx, v, w)
			if e != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				rpctrace.Add(ctx, rpctrace.StageRemoteAuthorityOmitted, 0)
				continue
			}
			entry, ok := deadlines[component.Key]
			if !ok {
				return j.fault(ErrCorrupt)
			}
			candidates = append(candidates, remoteVisible{value: RecordExport{Descriptor: descriptor(stored.Envelope, record, entry.ExpiresAt), Envelope: bytes.Clone(stored.Envelope)}, verified: v, window: grant})
		}
		committed, e = j.persistRead(ctx, checkpoint)
		if e != nil {
			return e
		}
		// Current authority is checked again after all persistence work. Returned
		// operation-local windows permit one final group expiry check.
		current := candidates[:0]
		for _, candidate := range candidates {
			v, e := r.verify(ctx, candidate.value.Envelope)
			if e != nil {
				if ctx.Err() != nil {
					return j.fault(ctx.Err())
				}
				if errors.Is(e, serviceorigin.ErrClock) {
					return j.fault(e)
				}
				rpctrace.Add(ctx, rpctrace.StageRemoteVerifyOmitted, 0)
				continue
			}
			w, e := j.current(ctx)
			if e != nil {
				return j.fault(e)
			}
			grant, e := r.authorize(ctx, v, w)
			if e != nil {
				if ctx.Err() != nil {
					return j.fault(ctx.Err())
				}
				rpctrace.Add(ctx, rpctrace.StageRemoteAuthorityOmitted, 0)
				continue
			}
			candidate.verified, candidate.window = v, grant
			current = append(current, candidate)
		}
		live, e = j.replica.Snapshot()
		if e != nil {
			return j.fault(e)
		}
		keys := make(map[world.Key]world.Version, len(live))
		for _, component := range live {
			keys[component.Key] = component.Version
		}
		final, e := j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		out = make([]RecordExport, 0, len(current))
		for _, candidate := range current {
			d := candidate.value.Descriptor
			// Keep the original short-circuit order. Counts identify only the
			// observed omitted branch, not an inferred cause or a new time read.
			if keys[d.Reference.Key] != d.Reference.Version {
				rpctrace.Add(ctx, rpctrace.StageRemoteMembershipOmitted, 0)
				continue
			}
			if !windowCurrent(candidate.window, final) {
				rpctrace.Add(ctx, rpctrace.StageRemoteWindowOmitted, 0)
				continue
			}
			if !recordCurrent(candidate.verified, final) {
				rpctrace.Add(ctx, rpctrace.StageRemoteRecordOmitted, 0)
				continue
			}
			rpctrace.Add(ctx, rpctrace.StageRemoteVisible, 0)
			out = append(out, candidate.value)
		}
		return nil
	})
	if err != nil {
		if committed {
			r.faulted.Store(true)
			return nil, errors.Join(ErrFaulted, err)
		}
		return nil, err
	}
	return out, nil
}
