package servicejournal

import (
	"bytes"
	"context"
	"encoding/hex"
	"sort"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func exportSources(j *Journal, sources []proofref.Source) ([]proofref.Source, error) {
	if j == nil || len(sources) == 0 || len(sources) > MaxInventoryEntries {
		return nil, serviceorigin.ErrDenied
	}
	out := append([]proofref.Source(nil), sources...)
	seen := map[proofref.Source]bool{}
	for _, source := range out {
		if source.Validate() != nil || source.Tenant != j.cfg.Replica.Tenant || source.Site != j.cfg.Replica.Site || seen[source] {
			return nil, serviceorigin.ErrDenied
		}
		seen[source] = true
	}
	return out, nil
}

// ExportInventory is historical repair, separate from Snapshot. Its descriptors
// include tombstones and expired samples. Current exchange permission is
// required even for an empty page; these hints grant no observation authority.
// after is an opaque lowercase full-key SHA256 from RecordDescriptor.OrderKey.
func (j *Journal) ExportInventory(ctx context.Context, sources []proofref.Source, after string, limit int, access ExportAccess) (page RecordPage, err error) {
	owned, e := exportSources(j, sources)
	if e != nil {
		return page, e
	}
	if limit < 1 || limit > MaxInventoryEntries || access.Authorize == nil {
		return page, ErrCapacity
	}
	if after != "" {
		b, e := hex.DecodeString(after)
		if e != nil || len(b) != 32 || hex.EncodeToString(b) != after {
			return page, ErrCapacity
		}
	}
	err = guarded(ctx, access.Guard, func() error {
		j.lock(ctx)
		defer j.mu.Unlock()
		if e := j.usable(); e != nil {
			return e
		}
		w, e := j.current(ctx)
		if e != nil {
			return e
		}
		initial, e := access.Authorize(ctx, append([]proofref.Source(nil), owned...), w)
		if e != nil {
			return e
		}
		if !windowCurrent(initial, w) {
			return serviceorigin.ErrExpired
		}
		checkpoint, e := j.replica.Checkpoint()
		if e != nil {
			return e
		}
		wanted := map[proofref.Source]bool{}
		for _, source := range owned {
			wanted[source] = true
		}
		all := make([]RecordDescriptor, 0, len(checkpoint.Entries))
		for _, entry := range checkpoint.Entries {
			proof, ok := j.proofs[componentKey(entry.Component.Key)]
			if !ok {
				return j.fault(ErrCorrupt)
			}
			_, record, e := j.decodeStored(proof.Envelope)
			if e != nil {
				return j.fault(e)
			}
			d := descriptor(proof.Envelope, record, entry.ExpiresAt)
			if d.Reference.Validate() != nil || d.ExpiresAtUnixNS <= 0 {
				return j.fault(ErrCorrupt)
			}
			if wanted[d.Reference.Source()] && d.OrderKey() > after {
				all = append(all, d)
			}
		}
		sort.Slice(all, func(a, b int) bool { return all[a].OrderKey() < all[b].OrderKey() })
		page.Entries = make([]RecordDescriptor, 0, min(limit, len(all)))
		for _, d := range all {
			if len(page.Entries) == limit {
				page.More = true
				break
			}
			trial := append(page.Entries, d)
			b, e := encode(trial)
			if e != nil {
				return e
			}
			if len(b) > 128<<10 {
				if len(page.Entries) == 0 {
					return ErrCapacity
				}
				page.More = true
				break
			}
			page.Entries = trial
		}
		if _, e = j.persistRead(ctx, checkpoint); e != nil {
			return e
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		grant, e := access.Authorize(ctx, append([]proofref.Source(nil), owned...), w)
		if e != nil {
			return e
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		if !windowCurrent(grant, w) {
			return serviceorigin.ErrExpired
		}
		return nil
	})
	if err != nil {
		return RecordPage{}, err
	}
	return page, nil
}

// ExportFetch returns exactly the requested historical version/deadline/hash,
// or found=false when a newer register or a shorter local cap superseded it.
// Only exact original envelope bytes leave storage; no re-signing occurs.
func (j *Journal) ExportFetch(ctx context.Context, wanted RecordDescriptor, access ExportAccess) (raw []byte, found bool, err error) {
	owned, e := exportSources(j, []proofref.Source{wanted.Reference.Source()})
	if e != nil {
		return nil, false, e
	}
	if wanted.Reference.Validate() != nil || wanted.ExpiresAtUnixNS <= 0 || access.Authorize == nil {
		return nil, false, serviceorigin.ErrDenied
	}
	err = guarded(ctx, access.Guard, func() error {
		j.lock(ctx)
		defer j.mu.Unlock()
		if e := j.usable(); e != nil {
			return e
		}
		w, e := j.current(ctx)
		if e != nil {
			return e
		}
		initial, e := access.Authorize(ctx, append([]proofref.Source(nil), owned...), w)
		if e != nil {
			return e
		}
		if !windowCurrent(initial, w) {
			return serviceorigin.ErrExpired
		}
		checkpoint, e := j.replica.Checkpoint()
		if e != nil {
			return e
		}
		for _, entry := range checkpoint.Entries {
			if entry.Component.Key != wanted.Reference.Key {
				continue
			}
			proof, ok := j.proofs[componentKey(entry.Component.Key)]
			if !ok {
				return j.fault(ErrCorrupt)
			}
			_, record, e := j.decodeStored(proof.Envelope)
			if e != nil {
				return j.fault(e)
			}
			if descriptor(proof.Envelope, record, entry.ExpiresAt) == wanted {
				raw = bytes.Clone(proof.Envelope)
				found = true
			}
			break
		}
		if _, e = j.persistRead(ctx, checkpoint); e != nil {
			return e
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		grant, e := access.Authorize(ctx, append([]proofref.Source(nil), owned...), w)
		if e != nil {
			return e
		}
		w, e = j.current(ctx)
		if e != nil {
			return j.fault(e)
		}
		if !windowCurrent(grant, w) {
			return serviceorigin.ErrExpired
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return raw, found, nil
}
