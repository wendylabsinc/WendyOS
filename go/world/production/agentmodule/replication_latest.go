package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxReplicaLatestKeys  = 256
	maxReplicaLatestBytes = 4 << 20
	// Two records plus one floor and lease per actor fit the eight-item RPC.
	maxReplicaLatestRecords = 2
)

type replicaLatestValue struct {
	item                     replication.Item
	scope                    serviceorigin.Scope
	delegationSHA256         string
	physicalUntil            time.Time
	sampleFrom, physicalFrom time.Time
	order                    uint64
}

type replicaLatestMark struct {
	version                    world.Version
	sampleUntil, physicalUntil time.Time
}

// This is a disposable latest-value queue, not another journal. Version marks
// remain after draining until their original deadlines, so late duplicate
// offers cannot retry a still-live unknown send. Expired marks are reclaimed
// for streams whose entity IDs change, such as anonymous person tracks.
// Capacity exhaustion drops fast-path values; ordinary repair remains separate.
type replicaLatestQueue struct {
	mu               sync.Mutex
	values           map[world.Key]replicaLatestValue
	versions         map[world.Key]replicaLatestMark
	bytes            int
	order            uint64
	sample, physical time.Time
}

func newReplicaLatestQueue() *replicaLatestQueue {
	return &replicaLatestQueue{values: map[world.Key]replicaLatestValue{}, versions: map[world.Key]replicaLatestMark{}}
}

func (q *replicaLatestQueue) pruneLocked(sample, physical time.Time) {
	if sample.After(q.sample) {
		q.sample = sample
	}
	if physical.After(q.physical) {
		q.physical = physical
	}
	for key, mark := range q.versions {
		if q.sample.Before(mark.sampleUntil) && q.physical.Before(mark.physicalUntil) {
			continue
		}
		delete(q.versions, key)
		if old, ok := q.values[key]; ok {
			q.bytes -= len(old.item.Envelope)
			delete(q.values, key)
		}
	}
}

func (q *replicaLatestQueue) advance(sample, physical time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked(sample, physical)
}

func (q *replicaLatestQueue) offer(v replicaLatestValue) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	// Lower bounds from an already-admitted original, not receipt timestamps
	// or a new clock. Take additionally supplies its existing fresh bounds.
	q.pruneLocked(v.sampleFrom, v.physicalFrom)
	sampleUntil := time.Unix(0, v.item.Entry.ExpiresAtUnixNS)
	if !q.sample.Before(sampleUntil) || !q.physical.Before(v.physicalUntil) {
		return false
	}
	key, version := v.item.Entry.Reference.Key, v.item.Entry.Reference.Version
	old, seen := q.versions[key]
	if seen && version.Compare(old.version) <= 0 || !seen && len(q.versions) >= maxReplicaLatestKeys {
		return false
	}
	mark := replicaLatestMark{version: version, sampleUntil: sampleUntil, physicalUntil: v.physicalUntil}
	if seen {
		// A newer value can have a shorter lifetime. Keep the prior consumed
		// version covered until its original bounds also expire; otherwise a
		// delayed older offer could revive after the newer mark is reclaimed.
		mark.sampleUntil = maxTime(mark.sampleUntil, old.sampleUntil)
		mark.physicalUntil = maxTime(mark.physicalUntil, old.physicalUntil)
	}
	q.versions[key] = mark
	if old, ok := q.values[key]; ok {
		q.bytes -= len(old.item.Envelope)
		delete(q.values, key)
	}
	if len(v.item.Envelope) > maxReplicaLatestBytes-q.bytes {
		return false
	}
	q.order++
	v.order = q.order
	v.item.Envelope = bytes.Clone(v.item.Envelope)
	q.values[key] = v
	q.bytes += len(v.item.Envelope)
	return true
}

func (q *replicaLatestQueue) take() ([]replicaLatestValue, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked(q.sample, q.physical)
	values := make([]replicaLatestValue, 0, len(q.values))
	for _, v := range q.values {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].order < values[j].order })
	if len(values) > maxReplicaLatestRecords {
		values = values[:maxReplicaLatestRecords]
	}
	for _, v := range values {
		delete(q.values, v.item.Entry.Reference.Key)
		q.bytes -= len(v.item.Envelope)
	}
	return values, len(q.values) != 0
}

func (m *Manager) signalReplicaLatest() {
	select {
	case m.replica.latestWake <- struct{}{}:
	default:
	}
}

// ReplicaLatestWake is a nonblocking notification, never a queued work count.
// The host fans it out to independently scheduled peer workers.
func (m *Manager) ReplicaLatestWake() <-chan struct{} {
	if m == nil || m.replica == nil {
		return nil
	}
	return m.replica.latestWake
}

// Only accept calls this, after its successful commit fence has returned.
// It parses owned originals but performs no clock, authority, store or network I/O.
func (m *Manager) offerReplicaLatest(signed serviceorigin.SignedRecord) {
	if m == nil || m.replica == nil {
		return
	}
	m.mu.RLock()
	ready := !m.closed && m.replica.started && !m.replica.failed.Load()
	m.mu.RUnlock()
	if !ready {
		return
	}
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	if jws.DecodeStrict(signed.Record, &record) != nil || jws.DecodeStrict(signed.Delegation.Claims, &delegation) != nil || record.Scope.Device != m.cfg.Policy.Device || !replication.SupportedComponent(record.Publication.ComponentType) {
		return
	}
	raw, err := json.Marshal(signed)
	if err != nil || len(raw) > replication.MaxEnvelopeBytes {
		return
	}
	component := (serviceorigin.VerifiedRecord{Record: record}).Component()
	sampleUntil, err := staticSampleDeadline(component)
	if err != nil {
		return
	}
	if component.Key.Type == sensorrecord.ComponentType && !component.Tombstone {
		sampleUntil, _, err = sensorrecord.LiveDeadlines(component)
		if err != nil {
			return
		}
	}
	physicalUntil := earlier(time.UnixMilli(record.ExpiresAtUnixMS), time.UnixMilli(delegation.ExpiresAtUnixMS))
	interpretation, err := replication.RecordInterpretationUntil(record)
	if err != nil {
		return
	}
	if !interpretation.IsZero() {
		physicalUntil = earlier(physicalUntil, interpretation)
	}
	v := replicaLatestValue{scope: record.Scope, delegationSHA256: record.DelegationSHA256, physicalUntil: physicalUntil,
		sampleFrom: record.Publication.Timing.SampleTime, physicalFrom: time.UnixMilli(record.IssuedAtUnixMS),
		item: replication.Item{Envelope: raw, Entry: replication.Entry{Kind: replication.Record,
			Reference:       exactReference(raw, serviceorigin.VerifiedRecord{Record: record, Bytes: signed.Record}),
			ExpiresAtUnixNS: sampleUntil.UnixNano(), Tombstone: record.Publication.Tombstone}}}
	if replication.ValidateItem(v.item, m.replicaEndpoint(), m.cfg.Policy.Device) != nil {
		return
	}
	wake := false
	for _, peer := range m.replica.peers {
		for _, source := range peer.policy.Export {
			if source == v.item.Entry.Reference.Source() {
				wake = peer.latest.offer(v) || wake
				break
			}
		}
	}
	if wake {
		m.signalReplicaLatest()
	}
}

func latestAuthorityItem(raw []byte, kind replication.Kind) (replication.Item, error) {
	item := replication.Item{Envelope: bytes.Clone(raw), Entry: replication.Entry{Kind: kind}}
	if kind == replication.Floor {
		_, c, err := sourceauthority.DecodeFloor(raw)
		if err != nil {
			return item, err
		}
		item.Entry.Authority = replication.AuthorityHint{Actor: replication.Actor{Tenant: c.Source.Tenant, Site: c.Site, Device: c.Source.Device, App: c.Source.App, Service: c.Source.Service}, Revision: c.AttestationRevision, Epoch: c.MinimumEpoch, ThroughRevision: c.ThroughRevision, EnvelopeSHA256: proofDigest(raw)}
	} else {
		_, c, err := sourceauthority.DecodeLease(raw)
		if err != nil {
			return item, err
		}
		item.Entry.Authority = replication.AuthorityHint{Actor: replication.Actor{Tenant: c.Scope.Tenant, Site: c.Scope.Site, Device: c.Scope.Device, App: c.Scope.App, Service: c.Scope.Service}, Revision: c.AuthorityRevision, Epoch: c.Epoch, EnvelopeSHA256: proofDigest(raw)}
	}
	return item, nil
}

// TakeReplicaLatest destructively drains pending values, then performs bounded
// local preparation under the existing replica fence. It never dials or sends.
// Expired, superseded-authority, oversized and failed preparations are dropped;
// their originals remain available to independent historical repair.
func (m *Manager) TakeReplicaLatest(ctx context.Context, device string) ([]replication.Item, error) {
	if ctx == nil || m == nil || m.replica == nil || m.replica.peers[device] == nil {
		return nil, ErrDenied
	}
	peer := m.replica.peers[device]
	values, more := peer.latest.take()
	if more {
		m.signalReplicaLatest()
	}
	if len(values) == 0 {
		return nil, nil
	}
	var out []replication.Item
	err := m.withReplica(ctx, func(ctx context.Context) error {
		states, err := m.replica.authority.Snapshot(ctx)
		if err != nil {
			return err
		}
		window, err := m.physicalNow()
		if err != nil {
			return err
		}
		sample := m.sampleNow(ctx)
		if sample.IsZero() {
			return ErrDenied
		}
		peer.latest.advance(sample, window.Latest)
		byKey := map[string]replication.Item{}
		used := 0
		for _, v := range values {
			if !sample.Before(time.Unix(0, v.item.Entry.ExpiresAtUnixNS)) || !window.Latest.Before(v.physicalUntil) {
				continue
			}
			for _, state := range states {
				if state.Source != sourceauthority.SourceOf(v.scope) || state.Epoch != v.item.Entry.Reference.Version.Epoch || state.Epoch < state.MinimumEpoch || state.LeaseRevision <= state.ThroughRevision || len(state.Lease) == 0 {
					continue
				}
				_, lease, err := sourceauthority.DecodeLease(state.Lease)
				if err != nil || !equalScopes(lease.Scope, v.scope) || lease.DelegationSHA256 != v.delegationSHA256 || window.Earliest.Before(time.UnixMilli(lease.NotBeforeUnixMS)) || !window.Latest.Before(time.UnixMilli(lease.NotAfterUnixMS)) {
					continue
				}
				candidate := []replication.Item{}
				for _, authority := range []struct {
					raw  []byte
					kind replication.Kind
				}{{state.Floor, replication.Floor}, {state.Lease, replication.Lease}} {
					if len(authority.raw) == 0 {
						continue
					}
					i, err := latestAuthorityItem(authority.raw, authority.kind)
					if err != nil || replication.ValidateItem(i, m.replicaEndpoint(), m.cfg.Policy.Device) != nil {
						return ErrDenied
					}
					candidate = append(candidate, i)
				}
				candidate = append(candidate, v.item)
				add := 0
				for _, item := range candidate {
					if _, exists := byKey[item.Entry.OrderKey()]; !exists {
						add += len(item.Envelope) + 1024
					}
				}
				if used+add <= replication.MaxPageBytes-4096 {
					for _, item := range candidate {
						byKey[item.Entry.OrderKey()] = item
					}
					used += add
				}
				break
			}
		}
		for _, item := range byKey {
			out = append(out, item)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Entry.OrderKey() < out[j].Entry.OrderKey() })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SendReplicaLatest does no preparation and holds no local admission locks
// across network I/O. The host owns this authenticated connection and retires
// the drained batch after any outcome, including an unobserved remote commit.
func (m *Manager) SendReplicaLatest(ctx context.Context, device string, connection replication.Connection, items []replication.Item) (replication.Result, error) {
	if ctx == nil || m == nil || m.replica == nil || ctx.Value(replicaOperationKey{}) != nil || connection.Identity.Device != device {
		return replication.Result{}, ErrDenied
	}
	m.mu.RLock()
	peer := m.replica.peers[device]
	ready := !m.closed && m.replica.started && !m.replica.failed.Load() && m.deps.Identity.Current(m.replica.identity.Token)
	m.mu.RUnlock()
	if !ready || peer == nil || peer.service == nil {
		return replication.Result{}, ErrDenied
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.replica.ctx, cancel)
	defer func() { stop(); cancel() }()
	return peer.service.SendLatest(ctx, connection, items)
}

func (d *replicaDispatcher) PushLatest(ctx context.Context, request *replicav1.PushLatestRequest) (*replicav1.PushLatestReply, error) {
	m := d.m
	m.mu.RLock()
	ready := m.replica != nil && m.replica.started && !m.closed && !m.replica.failed.Load()
	m.mu.RUnlock()
	if !ready || m.policyCurrent(ctx) != nil {
		return nil, status.Error(codes.PermissionDenied, "replication unavailable")
	}
	identity, err := m.replica.transport.IncomingPeer(ctx)
	if err != nil || m.replicaCurrent(ctx, identity, replication.Import, m.replicaGeneration(identity.Device)) != nil {
		return nil, status.Error(codes.PermissionDenied, "replica peer unavailable")
	}
	reply, err := m.replica.peers[identity.Device].service.PushLatest(ctx, request)
	if reply != nil && reply.UnknownOutcome {
		m.failReplication("latest_admission", replication.ErrUncertain)
	}
	return reply, err
}
