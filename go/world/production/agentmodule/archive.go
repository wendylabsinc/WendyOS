package agentmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofarchive"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func proofDigest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func exactReference(raw []byte, v serviceorigin.VerifiedRecord) proofref.Reference {
	c := v.Component()
	return proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: proofDigest(raw), RecordSHA256: proofDigest(v.Bytes)}
}
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func sourceSelector(ref proofref.Reference) serviceaccess.Source {
	return serviceaccess.Source{Device: ref.Key.Source.Device, App: ref.Key.Source.App, Service: ref.Key.Source.Service, ComponentType: ref.Key.Type, Stream: ref.Key.Source.Stream}
}

// A pin may survive an interrupted admission. It is bounded storage, never a
// successful publication. The host cannot acknowledge until both stores finish.
func (m *Manager) admitArchived(ctx context.Context, signed serviceorigin.SignedRecord) error {
	return m.admitArchivedReported(ctx, signed, nil)
}

// The optional report counts completed work in this exact admission, without a
// callback, additional read or exposed preparation/authority handle.
func (m *Manager) admitArchivedReported(ctx context.Context, signed serviceorigin.SignedRecord, report *preparedReport) error {
	refs, err := remoteReferences(signed, m.cfg.Policy.Device)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		return m.withReplica(ctx, func(ctx context.Context) error {
			child, err := m.rememberRemoteInputs(ctx, refs)
			if err != nil {
				return err
			}
			return m.admitArchivedLocal(child, signed, report)
		})
	}
	return m.admitArchivedLocal(ctx, signed, report)
}

func (m *Manager) admitArchivedLocal(ctx context.Context, signed serviceorigin.SignedRecord, report *preparedReport) error {
	if m.archive == nil {
		_, err := m.journal.Admit(ctx, signed)
		return err
	}
	raw, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	verified, err := m.verifyRecord(ctx, signed)
	if err != nil {
		return err
	}
	if err = m.archive.Prune(ctx); err != nil {
		return err
	}
	derived := verified.Record.Publication.ComponentType == resultrecord.ComponentType && !verified.Record.Publication.Tombstone
	if derived {
		result, e := resultrecord.Decode(verified.Record.Publication.Payload)
		if e != nil {
			return ErrDenied
		}
		var preparation *preparedPublication
		ctx, preparation, err = m.preparePublication(ctx, raw)
		if err != nil {
			return err
		}
		defer func() {
			preparation.close()
			if report != nil {
				*report = preparedReport{preparation.inputVerifications, preparation.mediaReads, preparation.currentChecks, preparation.closed}
			}
		}()
		if _, err = m.archive.Pin(ctx, proofarchive.PinRequest{ClaimEnvelope: raw, Inputs: result.Inputs, RetainUntilUnixMS: result.RetainUntilUnixMS}); err != nil {
			return err
		}
	}
	outcome, err := m.journal.Admit(ctx, signed)
	if err != nil {
		if derived {
			return errors.Join(proofarchive.ErrUncertain, err)
		}
		return err
	}
	if outcome != world.Applied && outcome != world.Duplicate {
		return ErrDenied
	}
	if _, err = m.archive.RememberAccepted(ctx, raw); err != nil {
		return errors.Join(proofarchive.ErrUncertain, err)
	}
	// Archive persistence and authorization are blocking work. Do not acknowledge
	// a source/result that expired while its proof was being retained.
	verified, err = m.verifyRecord(ctx, signed)
	if err == nil {
		err = m.admitSchema(ctx, verified)
	}
	if err == nil && derived {
		_, err = m.retainedResult(ctx, raw, verified)
	}
	if err == nil {
		var current []serviceorigin.SignedRecord
		current, err = m.filterCurrent(ctx, []serviceorigin.SignedRecord{signed})
		if err == nil && len(current) != 1 {
			err = ErrDenied
		}
	}
	if err != nil {
		return errors.Join(proofarchive.ErrUncertain, err)
	}
	return nil
}

type derivedInput struct {
	verified  serviceorigin.VerifiedRecord
	validated resultrecord.ValidatedInput
	remote    *remoteInputWindow
}
type derivedState struct {
	claim      serviceorigin.VerifiedRecord
	inputs     []derivedInput
	validation resultrecord.Validation
}

// This callback is invoked under the archive lock. It uses only supplied exact
// descriptors and independent authority/media stores, never journal/archive.
func (m *Manager) validatePin(ctx context.Context, claim serviceorigin.VerifiedRecord, inputs []proofarchive.Descriptor, until time.Time) error {
	state, err := m.derivedFromDescriptors(ctx, claim, inputs)
	if err != nil {
		return err
	}
	if state.validation.Result.RetainUntilUnixMS != until.UnixMilli() {
		return ErrDenied
	}
	return nil
}
func (m *Manager) derivedFromArchive(ctx context.Context, claim serviceorigin.VerifiedRecord) (derivedState, error) {
	if preparation, present := publicationPreparation(ctx); present {
		return preparation.current(ctx, m, claim)
	}
	if m.archive == nil {
		return derivedState{}, ErrDenied
	}
	result, err := resultrecord.Decode(claim.Record.Publication.Payload)
	if err != nil {
		return derivedState{}, ErrDenied
	}
	for _, ref := range result.Inputs {
		if ref.Key.Source.Device != m.cfg.Policy.Device && m.remoteOperation(ctx) == nil {
			var out derivedState
			err := m.withReplica(ctx, func(ctx context.Context) error {
				var e error
				out, e = m.derivedFromArchive(ctx, claim)
				return e
			})
			return out, err
		}
	}
	descriptors := make([]proofarchive.Descriptor, 0, len(result.Inputs))
	for _, ref := range result.Inputs {
		if ref.Key.Tenant != m.cfg.Policy.Tenant || ref.Key.Site != m.cfg.Policy.Site {
			return derivedState{}, ErrDenied
		}
		proof, e := m.archive.Get(ctx, ref.Source(), ref)
		if e != nil {
			return derivedState{}, e
		}
		descriptors = append(descriptors, proof.Descriptor)
	}
	return m.derivedFromDescriptors(ctx, claim, descriptors)
}
func (m *Manager) retainedResult(ctx context.Context, raw []byte, claim serviceorigin.VerifiedRecord) (derivedState, error) {
	if m.archive == nil {
		return derivedState{}, ErrDenied
	}
	result, err := resultrecord.Decode(claim.Record.Publication.Payload)
	if err != nil {
		return derivedState{}, ErrDenied
	}
	pin, err := m.archive.CheckPin(ctx, exactReference(raw, claim))
	if err != nil {
		return derivedState{}, err
	}
	if pin.RetainUntilUnixMS != result.RetainUntilUnixMS || !reflect.DeepEqual(pin.Inputs, result.Inputs) {
		return derivedState{}, ErrDenied
	}
	return m.derivedFromArchive(ctx, claim)
}
func (m *Manager) buildDerivedFromDescriptors(ctx context.Context, claim serviceorigin.VerifiedRecord, descriptors []proofarchive.Descriptor) (derivedState, error) {
	state := derivedState{claim: claim}
	if claim.Record.Publication.ComponentType != resultrecord.ComponentType || claim.Record.Publication.Tombstone {
		return state, ErrDenied
	}
	result, err := resultrecord.Decode(claim.Record.Publication.Payload)
	if err != nil || len(result.Inputs) != len(descriptors) {
		return state, ErrDenied
	}
	inputs := make([]resultrecord.ValidatedInput, 0, len(descriptors))
	for _, d := range descriptors {
		if d.Reference.Validate() != nil || d.Reference.Key.Tenant != m.cfg.Policy.Tenant || d.Reference.Key.Site != m.cfg.Policy.Site || proofDigest(d.Envelope) != d.Reference.EnvelopeSHA256 {
			return state, ErrDenied
		}
		var signed serviceorigin.SignedRecord
		if json.Unmarshal(d.Envelope, &signed) != nil {
			return state, ErrDenied
		}
		remote := d.Reference.Key.Source.Device != m.cfg.Policy.Device
		verify := m.verifyRecord
		if remote {
			if m.remoteVerifier == nil || m.remoteOperation(ctx) == nil {
				return state, ErrDenied
			}
			verify = m.remoteVerifier.Verify
		}
		v, e := verify(ctx, signed)
		if e != nil || exactReference(d.Envelope, v) != d.Reference {
			return state, ErrDenied
		}
		if preparation, present := publicationPreparation(ctx); present {
			preparation.inputVerifications++
		}
		c := v.Component()
		// Reject recursion before invoking schema, which may itself resolve inputs.
		if c.Tombstone || c.Key.Type == resultrecord.ComponentType || c.Provenance.Origin != world.Measured && c.Provenance.Origin != world.Simulated {
			return state, ErrDenied
		}
		var remoteWindow *remoteInputWindow
		if remote {
			remoteWindow, e = m.remoteInputAuthority(ctx, v)
			if e == nil {
				e = m.validateRemoteInputMedia(ctx, v)
			}
		} else {
			e = m.admitSchema(ctx, v)
		}
		if e != nil {
			return state, e
		}
		sampleUntil, physicalUntil, e := m.inputDeadlines(ctx, v, result.RetainUntilUnixMS)
		if e != nil {
			return state, e
		}
		input := resultrecord.ValidatedInput{Reference: d.Reference, Component: c, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil}
		inputs = append(inputs, input)
		state.inputs = append(state.inputs, derivedInput{verified: v, validated: input, remote: remoteWindow})
	}
	state.validation, err = resultrecord.ValidatePublication(claim.Record.Publication, inputs)
	if err == nil {
		err = applyHazardPhysicalCap(&state.validation)
	}
	if err != nil {
		return state, ErrDenied
	}
	if m.policyCurrent(ctx) != nil {
		return state, ErrDenied
	}
	sampleNow := m.sampleNow(ctx)
	window, err := m.physicalNow()
	if err != nil {
		return state, ErrDenied
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.policyWindowCurrent(window) || !m.derivedCurrentLocked(ctx, state, sampleNow, window) {
		return state, ErrDenied
	}
	return state, nil
}

// New hazard payloads carry the original physical capture deadline for remote
// readers. Local admission must use the same deadline; an app cannot extend
// the host's independently computed input bound. Older local payloads omit it.
func applyHazardPhysicalCap(v *resultrecord.Validation) error {
	if v.Result.ResultSchema != fusion.HazardType {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(v.Result.ResultJSON, &fields) != nil {
		return ErrDenied
	}
	raw, present := fields["physical_valid_until"]
	if !present {
		return nil
	}
	var cap time.Time
	if json.Unmarshal(raw, &cap) != nil || cap.IsZero() || cap.After(v.PhysicalValidUntil) {
		return ErrDenied
	}
	v.PhysicalValidUntil = cap
	return nil
}

func staticSampleDeadline(c world.Component) (time.Time, error) {
	if !c.Timing.ErrorKnown || c.TTL <= 0 || c.Timing.ErrorBound >= c.TTL || c.Timing.AgeHint >= c.TTL {
		return time.Time{}, ErrDenied
	}
	margin := c.Timing.ErrorBound
	if c.Timing.AgeHint > margin {
		margin = c.Timing.AgeHint
	}
	return c.Timing.SampleTime.Add(c.TTL - margin), nil
}
func authorityDeadline(v serviceorigin.VerifiedRecord) time.Time {
	return earlier(earlier(time.UnixMilli(v.Record.ExpiresAtUnixMS), time.UnixMilli(v.Delegation.ExpiresAtUnixMS)), v.Device.ValidUntil)
}
func (m *Manager) inputDeadlines(ctx context.Context, v serviceorigin.VerifiedRecord, retainUntil int64) (time.Time, time.Time, error) {
	c := v.Component()
	sampleUntil, err := staticSampleDeadline(c)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	physicalUntil := authorityDeadline(v)
	var sha, contentType string
	var size int64
	if c.Key.Type == sensorrecord.ComponentType {
		var physical time.Time
		sampleUntil, physical, err = sensorrecord.LiveDeadlines(c)
		if err != nil {
			return time.Time{}, time.Time{}, ErrDenied
		}
		physicalUntil = earlier(physicalUntil, physical)
		capture, e := sensorrecord.FromComponent(c)
		if e != nil {
			return time.Time{}, time.Time{}, ErrDenied
		}
		ref, e := capture.Evidence()
		if e != nil {
			return time.Time{}, time.Time{}, ErrDenied
		}
		if ref != nil {
			sha, contentType, size = ref.SHA256, ref.ContentType, ref.Size
		}
	} else if c.Key.Type == "wendy.sensor.image.v1" || c.Key.Type == "wendy.sensor.audio.v1" {
		var media mediaPayload
		if jws.DecodeStrict(c.Payload, &media) != nil {
			return time.Time{}, time.Time{}, ErrDenied
		}
		sha, contentType, size = media.SHA256, media.ContentType, media.Size
		if c.Key.Type == "wendy.sensor.audio.v1" {
			sampleUntil = sampleUntil.Add(-media.Duration)
		}
	}
	if sha != "" {
		owner := m.owner(sourceSelector(proofref.Reference{Key: c.Key}))
		retained, e := m.matchInputMedia(ctx, owner, sha, size, contentType)
		physicalUntil = earlier(physicalUntil, time.UnixMilli(retained.RetainUntilUnixMS))
		if e != nil || retainUntil > retained.RetainUntilUnixMS {
			return time.Time{}, time.Time{}, ErrDenied
		}
	}
	if !sampleUntil.After(c.Timing.SampleTime) {
		return time.Time{}, time.Time{}, ErrDenied
	}
	return sampleUntil, physicalUntil, nil
}
func (m *Manager) recordCurrentLocked(r serviceorigin.Record, sampleUntil, physicalUntil, sampleNow time.Time, window trustedtime.Interval) bool {
	if m.closed || m.policyFailed.Load() || sampleNow.IsZero() || window.Validate() != nil {
		return false
	}
	b := m.bindings[bindingKey(r.Scope.App, r.Scope.Service)]
	return b != nil && equalScopes(b.scope, r.Scope) && b.epoch == r.Epoch && m.deps.Identity.Current(b.token) &&
		!r.Publication.Timing.SampleTime.After(sampleNow) && sampleNow.Before(sampleUntil) &&
		!window.Earliest.Before(time.UnixMilli(r.IssuedAtUnixMS)) && window.Latest.Before(physicalUntil)
}
func (m *Manager) derivedCurrentLocked(ctx context.Context, state derivedState, sampleNow time.Time, window trustedtime.Interval) bool {
	if ctx.Err() != nil || !window.Latest.Before(time.UnixMilli(state.validation.Result.RetainUntilUnixMS)) || !m.recordCurrentLocked(state.claim.Record, state.validation.SampleValidUntil, earlier(authorityDeadline(state.claim), state.validation.PhysicalValidUntil), sampleNow, window) {
		return false
	}
	claimant := m.bindings[bindingKey(state.claim.Record.Scope.App, state.claim.Record.Scope.Service)]
	if m.identityAllowed(state.claim.Device) != nil {
		return false
	}
	for _, input := range state.inputs {
		if !subscribed(claimant, sourceSelector(input.validated.Reference)) {
			return false
		}
		if input.remote != nil {
			if !m.remoteInputCurrent(ctx, input, sampleNow, window) {
				return false
			}
		} else if m.identityAllowed(input.verified.Device) != nil || !m.recordCurrentLocked(input.verified.Record, input.validated.SampleValidUntil, input.validated.PhysicalValidUntil, sampleNow, window) {
			return false
		}
	}
	return true
}
