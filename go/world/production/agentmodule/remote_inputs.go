package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// A remote input keeps its original identity and lifetime. These bounds are
// usable only while the existing replica operation holds the peer/lease fence.
type remoteInputWindow struct {
	operation   *replicaOperation
	from, until time.Time
}

func (m *Manager) remoteOperation(ctx context.Context) *replicaOperation {
	if ctx == nil || ctx.Err() != nil || m.replica == nil || m.replica.failed.Load() {
		return nil
	}
	op, _ := ctx.Value(replicaOperationKey{}).(*replicaOperation)
	if op == nil || op.manager != m || !op.remote || !op.active.Load() || !m.deps.Identity.Current(op.token) {
		return nil
	}
	return op
}

func (m *Manager) authorizeRemoteDelegation(ctx context.Context, c serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
	if m.remoteOperation(ctx) == nil || m.policyCurrent(ctx) != nil || c.Scope.Device == m.cfg.Policy.Device || id.Principal != c.Scope.Device || m.replicaIdentityAllowed(id) != nil {
		return ErrDenied
	}
	for _, p := range c.Scope.Publish {
		s := serviceaccess.Source{Device: c.Scope.Device, App: c.Scope.App, Service: c.Scope.Service, ComponentType: p.ComponentType, Stream: p.Stream}
		if !m.cfg.Replication.imports(m.cfg.Policy, s) {
			return ErrDenied
		}
	}
	return m.replica.authority.AuthorizeDelegation(ctx, c, id)
}

func (m *Manager) remoteInputAuthority(ctx context.Context, v serviceorigin.VerifiedRecord) (*remoteInputWindow, error) {
	op := m.remoteOperation(ctx)
	r := v.Record
	if op == nil || r.Scope.Device == m.cfg.Policy.Device || r.Publication.ComponentType != sensorrecord.ComponentType || r.Publication.Tombstone || m.replicaIdentityAllowed(v.Device) != nil {
		return nil, ErrDenied
	}
	if err := m.admitReplicaSchema(ctx, v); err != nil {
		return nil, err
	}
	lease, err := m.replica.authority.CurrentLease(ctx, sourceauthority.SourceOf(r.Scope))
	if err != nil || m.replica.authority.AuthorizeRecord(ctx, lease, v) != nil {
		return nil, ErrDenied
	}
	var c sourceauthority.LeaseClaims
	if jws.DecodeStrict(lease.Claims, &c) != nil {
		return nil, ErrDenied
	}
	peer := m.replica.peers[r.Scope.Device]
	if peer == nil {
		return nil, ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil {
		return nil, err
	}
	pw, err := m.replicaPeerWindow(ctx, replication.Identity{Device: r.Scope.Device, BindingSHA256: peer.policy.CredentialSHA256}, peer.policy.Generation, replication.Import, peer.policy.Import, w)
	if err != nil {
		return nil, err
	}
	return &remoteInputWindow{operation: op,
		from:  maxTime(time.UnixMilli(r.IssuedAtUnixMS), time.UnixMilli(v.Delegation.IssuedAtUnixMS), time.UnixMilli(c.NotBeforeUnixMS), pw.NotBefore),
		until: earlier(authorityDeadline(v), earlier(time.UnixMilli(c.NotAfterUnixMS), pw.NotAfter))}, nil
}

func (m *Manager) remoteInputCurrent(ctx context.Context, input derivedInput, sample time.Time, w trustedtime.Interval) bool {
	r := input.verified.Record
	p := input.remote
	return p != nil && m.remoteOperation(ctx) == p.operation && m.replicaIdentityAllowed(input.verified.Device) == nil &&
		!sample.IsZero() && !r.Publication.Timing.SampleTime.After(sample) && sample.Before(input.validated.SampleValidUntil) &&
		!w.Earliest.Before(p.from) && w.Latest.Before(earlier(p.until, input.validated.PhysicalValidUntil))
}

func remoteReferences(signed serviceorigin.SignedRecord, device string) ([]proofref.Reference, error) {
	var r serviceorigin.Record
	if jws.DecodeStrict(signed.Record, &r) != nil {
		return nil, ErrDenied
	}
	if r.Publication.ComponentType != resultrecord.ComponentType || r.Publication.Tombstone {
		return nil, nil
	}
	claim, err := resultrecord.Decode(r.Publication.Payload)
	if err != nil {
		return nil, ErrDenied
	}
	var refs []proofref.Reference
	for _, ref := range claim.Inputs {
		if ref.Key.Source.Device != device {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

type remoteAcceptedKey struct{}
type remoteAcceptedInputs struct {
	operation *replicaOperation
	envelopes map[proofref.Reference][]byte
}

// Resolve originals from the native replica before entering the archive. No
// app can import an arbitrary signed blob through this host-only path.
func (m *Manager) rememberRemoteInputs(ctx context.Context, refs []proofref.Reference) (context.Context, error) {
	op := m.remoteOperation(ctx)
	if op == nil || m.archive == nil {
		return nil, ErrDenied
	}
	accepted := &remoteAcceptedInputs{operation: op, envelopes: map[proofref.Reference][]byte{}}
	byDevice := map[string]bool{}
	for _, ref := range refs {
		if ref.Validate() != nil || ref.Key.Tenant != m.cfg.Policy.Tenant || ref.Key.Site != m.cfg.Policy.Site || ref.Key.Type != sensorrecord.ComponentType {
			return nil, ErrDenied
		}
		byDevice[ref.Key.Source.Device] = true
	}
	for device := range byDevice {
		peer := m.replica.peers[device]
		if peer == nil {
			return nil, ErrDenied
		}
		values, err := peer.store.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			for _, ref := range refs {
				if ref.Key.Source.Device != device || proofDigest(value.Envelope) != ref.EnvelopeSHA256 {
					continue
				}
				var signed serviceorigin.SignedRecord
				if json.Unmarshal(value.Envelope, &signed) != nil {
					return nil, ErrDenied
				}
				v, err := m.remoteVerifier.Verify(ctx, signed)
				if err != nil || exactReference(value.Envelope, v) != ref {
					return nil, ErrDenied
				}
				accepted.envelopes[ref] = bytes.Clone(value.Envelope)
			}
		}
	}
	child := context.WithValue(ctx, remoteAcceptedKey{}, accepted)
	for _, ref := range refs {
		raw, ok := accepted.envelopes[ref]
		if !ok {
			return nil, ErrDenied
		}
		if _, err := m.archive.RememberRemoteAccepted(child, raw); err != nil {
			return nil, err
		}
	}
	return child, nil
}

// Archive callbacks never query a journal or acquire the replica fence. They
// check the exact pre-resolved original inside the already-held operation.
func (m *Manager) validateRemoteAccepted(ctx context.Context, v serviceorigin.VerifiedRecord, raw []byte) error {
	a, _ := ctx.Value(remoteAcceptedKey{}).(*remoteAcceptedInputs)
	if a == nil || m.remoteOperation(ctx) != a.operation || !bytes.Equal(a.envelopes[exactReference(raw, v)], raw) {
		return ErrDenied
	}
	p, err := m.remoteInputAuthority(ctx, v)
	if err != nil {
		return err
	}
	sampleUntil, physicalUntil, err := m.inputDeadlines(ctx, v, 0)
	if err != nil {
		return err
	}
	sample := m.sampleNow(ctx)
	w, err := m.physicalNow()
	input := derivedInput{verified: v, remote: p, validated: resultrecord.ValidatedInput{SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil}}
	if err != nil || !m.remoteInputCurrent(ctx, input, sample, w) {
		return ErrDenied
	}
	return nil
}

func (m *Manager) needsReplicaForRead(proofs []serviceorigin.SignedRecord) bool {
	for _, proof := range proofs {
		refs, err := remoteReferences(proof, m.cfg.Policy.Device)
		if err != nil || len(refs) != 0 {
			return true
		}
	}
	return false
}
