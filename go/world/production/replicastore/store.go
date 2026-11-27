package replicastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func actor(e replication.Entry) replication.Actor {
	if e.Kind != replication.Record {
		return e.Authority.Actor
	}
	k := e.Reference.Key
	return replication.Actor{Tenant: k.Tenant, Site: k.Site, Device: k.Source.Device, App: k.Source.App, Service: k.Source.Service}
}
func authoritySource(a replication.Actor) sourceauthority.Source {
	return sourceauthority.Source{Tenant: a.Tenant, Device: a.Device, App: a.App, Service: a.Service}
}
func descriptor(e replication.Entry) servicejournal.RecordDescriptor {
	return servicejournal.RecordDescriptor{Reference: e.Reference, ExpiresAtUnixNS: e.ExpiresAtUnixNS, Tombstone: e.Tombstone}
}
func recordEntry(d servicejournal.RecordDescriptor) replication.Entry {
	return replication.Entry{Kind: replication.Record, Reference: d.Reference, ExpiresAtUnixNS: d.ExpiresAtUnixNS, Tombstone: d.Tombstone}
}
func (s *Store) hintSources(d replication.Direction, e replication.Entry) ([]proofref.Source, error) {
	a := actor(e)
	src := authoritySource(a)
	origin := s.cfg.Peer.Device
	list := s.cfg.Import
	if d == replication.Export {
		origin = s.cfg.Endpoint.Device
		list = s.cfg.Export
	}
	if src.Validate() != nil || a.Tenant != s.cfg.Endpoint.Tenant || a.Site != s.cfg.Endpoint.Site || a.Device != origin {
		return nil, ErrDenied
	}
	if e.Kind == replication.Record {
		if e.Authority != (replication.AuthorityHint{}) || e.Reference.Validate() != nil || e.ExpiresAtUnixNS <= 0 {
			return nil, ErrDenied
		}
		sources := []proofref.Source{e.Reference.Source()}
		if !s.sourcesAllowed(d, sources) {
			return nil, ErrDenied
		}
		return sources, nil
	}
	h := e.Authority
	if e.Kind != replication.Floor && e.Kind != replication.Lease || e.Reference != (proofref.Reference{}) || e.ExpiresAtUnixNS != 0 || e.Tombstone || h.Revision == 0 || h.Epoch == 0 || !digestValid(h.EnvelopeSHA256) || e.Kind == replication.Floor && (h.ThroughRevision == 0 || h.ThroughRevision > h.Revision) || e.Kind == replication.Lease && h.ThroughRevision != 0 {
		return nil, ErrDenied
	}
	var out []proofref.Source
	for _, p := range list {
		if p.App == a.App && p.Service == a.Service {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, ErrDenied
	}
	return out, nil
}
func (s *Store) authorityItem(raw []byte, kind replication.Kind) (replication.Item, error) {
	i := replication.Item{Envelope: bytes.Clone(raw), Entry: replication.Entry{Kind: kind}}
	if kind == replication.Floor {
		_, c, e := sourceauthority.DecodeFloor(raw)
		if e != nil {
			return i, e
		}
		i.Entry.Authority = replication.AuthorityHint{Actor: replication.Actor{Tenant: c.Source.Tenant, Site: c.Site, Device: c.Source.Device, App: c.Source.App, Service: c.Source.Service}, Revision: c.AttestationRevision, Epoch: c.MinimumEpoch, ThroughRevision: c.ThroughRevision, EnvelopeSHA256: hash(raw)}
	} else {
		_, c, e := sourceauthority.DecodeLease(raw)
		if e != nil {
			return i, e
		}
		i.Entry.Authority = replication.AuthorityHint{Actor: replication.Actor{Tenant: c.Scope.Tenant, Site: c.Scope.Site, Device: c.Scope.Device, App: c.Scope.App, Service: c.Scope.Service}, Revision: c.AuthorityRevision, Epoch: c.Epoch, EnvelopeSHA256: hash(raw)}
	}
	return i, nil
}
func (s *Store) exportAccess() servicejournal.ExportAccess {
	return servicejournal.ExportAccess{Guard: s.journalGuard, Authorize: func(ctx context.Context, p []proofref.Source, _ trustedtime.Interval) (servicejournal.AuthorityWindow, error) {
		return s.check(ctx, replication.Export, p)
	}}
}
func validAfter(after string) bool {
	return after == "" || len(after) == 66 && after[1] == ':' && after[0] >= '1' && after[0] <= '3' && digestValid(after[2:])
}
func eligible(r sourceauthority.State) bool {
	return len(r.Lease) > 0 && r.LeaseRevision > r.ThroughRevision && r.Epoch >= r.MinimumEpoch
}
func stateActor(p proofref.Source) sourceauthority.Source {
	return sourceauthority.Source{Tenant: p.Tenant, Device: p.Device, App: p.App, Service: p.Service}
}
func (s *Store) Inventory(ctx context.Context, sources []proofref.Source, after string, limit int) (out replication.InventoryPage, err error) {
	sources = append([]proofref.Source(nil), sources...)
	if limit < 1 || limit > replication.MaxInventoryEntries || !validAfter(after) {
		return out, replication.ErrProtocol
	}
	err = s.guard(ctx, func(ctx context.Context) error {
		if _, e := s.check(ctx, replication.Export, sources); e != nil {
			return e
		}
		if len(sources) == 0 {
			return nil
		}
		states, e := s.cfg.Authority.Snapshot(ctx)
		if e != nil {
			return e
		}
		var entries []replication.Entry
		liveActors := map[sourceauthority.Source]uint64{}
		for _, state := range states {
			if state.Source.Device != s.cfg.Endpoint.Device {
				continue
			}
			wanted := false
			for _, p := range sources {
				if p.Tenant == state.Source.Tenant && p.App == state.Source.App && p.Service == state.Source.Service {
					wanted = true
					break
				}
			}
			if !wanted {
				continue
			}
			if eligible(state) {
				liveActors[state.Source] = state.Epoch
			}
			for _, value := range []struct {
				kind replication.Kind
				raw  []byte
			}{{replication.Floor, state.Floor}, {replication.Lease, state.Lease}} {
				if len(value.raw) == 0 {
					continue
				}
				if value.kind == replication.Lease && !eligible(state) {
					continue
				}
				i, e := s.authorityItem(value.raw, value.kind)
				if e != nil {
					return e
				}
				if replication.ValidateItem(i, s.cfg.Endpoint, s.cfg.Endpoint.Device) != nil {
					return ErrDenied
				}
				if i.Entry.OrderKey() > after {
					entries = append(entries, i.Entry)
				}
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].OrderKey() < entries[j].OrderKey() })
		more := len(entries) > limit
		if len(entries) <= limit && len(liveActors) > 0 {
			recordAfter := ""
			if strings.HasPrefix(after, "3:") {
				recordAfter = after[2:]
			}
			var selected []proofref.Source
			for _, p := range sources {
				if liveActors[stateActor(p)] != 0 {
					selected = append(selected, p)
				}
			}
			// Local journals contain at most 4096 registers. Scan bounded pages
			// past older source epochs; a new lease cannot authorize those bytes.
			for scanned := 0; scanned < 4096; {
				page, e := s.cfg.Local.ExportInventory(ctx, selected, recordAfter, 64, s.exportAccess())
				if e != nil {
					return e
				}
				if len(page.Entries) == 0 && page.More {
					return ErrDenied
				}
				for _, d := range page.Entries {
					if d.OrderKey() <= recordAfter {
						return ErrDenied
					}
					recordAfter = d.OrderKey()
					scanned++
					entry := recordEntry(d)
					if _, e := s.hintSources(replication.Export, entry); e != nil {
						return e
					}
					if d.Reference.Version.Epoch == liveActors[stateActor(d.Reference.Source())] {
						entries = append(entries, entry)
					}
					if len(entries) > limit {
						break
					}
				}
				more = page.More || len(entries) > limit
				if len(entries) >= limit || !page.More {
					break
				}
				if scanned == 4096 {
					return replication.ErrCapacity
				}
			}
		}
		if len(entries) > limit {
			entries = entries[:limit]
		}
		if _, e := s.check(ctx, replication.Export, sources); e != nil {
			return e
		}
		out = replication.InventoryPage{Entries: entries, More: more}
		return nil
	})
	if err != nil {
		return replication.InventoryPage{}, err
	}
	return out, nil
}
func (s *Store) Fetch(ctx context.Context, e replication.Entry) (item replication.Item, found bool, err error) {
	err = s.guard(ctx, func(ctx context.Context) error {
		sources, x := s.hintSources(replication.Export, e)
		if x != nil {
			return x
		}
		if _, x = s.check(ctx, replication.Export, sources); x != nil {
			return x
		}
		states, x := s.cfg.Authority.Snapshot(ctx)
		if x != nil {
			return x
		}
		var state sourceauthority.State
		for _, candidate := range states {
			if candidate.Source == authoritySource(actor(e)) {
				state = candidate
				break
			}
		}
		if e.Kind == replication.Record {
			if !eligible(state) || e.Reference.Version.Epoch != state.Epoch {
				_, x = s.check(ctx, replication.Export, sources)
				return x
			}
			raw, ok, x := s.cfg.Local.ExportFetch(ctx, descriptor(e), s.exportAccess())
			if x != nil {
				return x
			}
			if ok {
				item = replication.Item{Entry: e, Envelope: raw}
				found = true
			}
		} else {
			for _, state := range states {
				if state.Source != authoritySource(actor(e)) {
					continue
				}
				if e.Kind == replication.Lease && !eligible(state) {
					break
				}
				raw := state.Lease
				if e.Kind == replication.Floor {
					raw = state.Floor
				}
				if len(raw) == 0 {
					break
				}
				candidate, x := s.authorityItem(raw, e.Kind)
				if x != nil {
					return x
				}
				if candidate.Entry == e {
					item, found = candidate, true
				}
				break
			}
		}
		if found && replication.ValidateItem(item, s.cfg.Endpoint, s.cfg.Endpoint.Device) != nil {
			return ErrDenied
		}
		_, x = s.check(ctx, replication.Export, sources)
		return x
	})
	if err != nil {
		return replication.Item{}, false, err
	}
	return item, found, nil
}
func (s *Store) Need(ctx context.Context, e replication.Entry) (need bool, err error) {
	err = s.guard(ctx, func(ctx context.Context) error {
		sources, x := s.hintSources(replication.Import, e)
		if x != nil {
			return x
		}
		if _, x = s.check(ctx, replication.Import, sources); x != nil {
			return x
		}
		if e.Kind == replication.Record {
			need, x = s.remote.Need(ctx, descriptor(e))
			if x != nil {
				return x
			}
		} else {
			need = true
			states, x := s.cfg.Authority.Snapshot(ctx)
			if x != nil {
				return x
			}
			for _, state := range states {
				if state.Source != authoritySource(actor(e)) {
					continue
				}
				rev, raw := state.LeaseRevision, state.Lease
				if e.Kind == replication.Floor {
					rev, raw = state.FloorRevision, state.Floor
				} else if e.Authority.Revision <= state.ThroughRevision || e.Authority.Epoch < state.MinimumEpoch || e.Authority.Epoch < state.Epoch {
					need = false
					break
				}
				if e.Authority.Revision < rev {
					need = false
				} else if e.Authority.Revision == rev && len(raw) > 0 {
					original, x := s.authorityItem(raw, e.Kind)
					if x != nil {
						return x
					}
					need = original.Entry != e
				}
				break
			}
		}
		_, x = s.check(ctx, replication.Import, sources)
		return x
	})
	if err != nil {
		return false, err
	}
	return need, nil
}

// RemoteAuthority is the transport's peer/scope policy hook. Actual signatures,
// source leases, original windows and schema are checked by the durable stores.
// The private nested context permits this hook at a journal final boundary.
func (s *Store) RemoteAuthority(ctx context.Context, item replication.Item) error {
	run := func(ctx context.Context) error {
		sources, e := s.hintSources(replication.Import, item.Entry)
		if e != nil {
			return e
		}
		remote := s.cfg.Endpoint
		remote.Device = s.cfg.Peer.Device
		if replication.ValidateItem(item, remote, s.cfg.Peer.Device) != nil {
			return ErrDenied
		}
		var original *servicejournal.AuthorityWindow
		if item.Entry.Kind == replication.Lease {
			_, c, e := sourceauthority.DecodeLease(item.Envelope)
			if e != nil || !s.scopeAllowed(c.Scope) {
				return ErrDenied
			}
			original = &servicejournal.AuthorityWindow{NotBefore: time.UnixMilli(c.NotBeforeUnixMS), NotAfter: time.UnixMilli(c.NotAfterUnixMS)}
		} else if item.Entry.Kind == replication.Floor {
			_, c, e := sourceauthority.DecodeFloor(item.Envelope)
			if e != nil {
				return e
			}
			original = &servicejournal.AuthorityWindow{NotBefore: time.UnixMilli(c.NotBeforeUnixMS), NotAfter: time.UnixMilli(c.NotAfterUnixMS)}
		}
		_, e = s.checkWindow(ctx, replication.Import, sources, original)
		return e
	}
	if s.inside(ctx) {
		return run(ctx)
	}
	return s.guard(ctx, run)
}
func (s *Store) Admit(ctx context.Context, item replication.Item, finalCheck func(context.Context) error) (err error) {
	if finalCheck == nil {
		return ErrDenied
	}
	item.Envelope = bytes.Clone(item.Envelope)
	completed := false
	recordWindowRefused := false
	err = s.guard(ctx, func(ctx context.Context) error {
		if e := s.RemoteAuthority(ctx, item); e != nil {
			return e
		}
		if item.Entry.Kind == replication.Record {
			_, e := s.remote.AdmitRemote(ctx, item.Envelope, item.Entry.ExpiresAtUnixNS, finalCheck)
			completed = e == nil
			recordWindowRefused = servicejournal.IsRecordWindowRefusal(e)
			return e
		}
		if e := finalCheck(ctx); e != nil {
			return e
		}
		if item.Entry.Kind == replication.Lease {
			p, _, e := sourceauthority.DecodeLease(item.Envelope)
			if e != nil {
				return e
			}
			if e = s.cfg.Authority.AcceptLease(ctx, p); e != nil {
				return e
			}
		} else {
			p, _, e := sourceauthority.DecodeFloor(item.Envelope)
			if e != nil {
				return e
			}
			if e = s.cfg.Authority.AcceptFloor(ctx, p); e != nil {
				return e
			}
		}
		completed = true
		if e := finalCheck(ctx); e != nil {
			return e
		}
		return s.RemoteAuthority(ctx, item)
	})
	if err != nil && (completed || errors.Is(err, sourceauthority.ErrFaulted) || errors.Is(err, servicejournal.ErrFaulted)) {
		return s.mutationFault(err)
	}
	if refused, ok := err.(*guardNotEntered); ok && s.usable() == nil &&
		!errors.Is(err, ErrFaulted) && !errors.Is(err, replication.ErrUncertain) {
		return replication.AdmissionNotStarted(refused.cause)
	}
	if err != nil && recordWindowRefused && servicejournal.IsRecordWindowRefusal(err) &&
		ctx.Err() == nil && s.usable() == nil {
		return errors.Join(replication.ErrRecordWindowDeferred, err)
	}
	return err
}

// Snapshot is for an independently authorized ordinary-reader integration.
// This adapter still requires its exact peer/source policy and remote lease.
type snapshotContextKey struct{}

func (s *Store) Snapshot(ctx context.Context) (out []servicejournal.RecordExport, err error) {
	err = s.guard(ctx, func(ctx context.Context) error {
		if _, e := s.check(ctx, replication.Import, s.cfg.Import); e != nil {
			return e
		}
		var e error
		out, e = s.remote.Snapshot(context.WithValue(ctx, snapshotContextKey{}, s))
		if e != nil {
			return e
		}
		// RemoteJournal already checks the complete returned group at its last
		// source/physical observations. Do not add blocking work after that.
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
