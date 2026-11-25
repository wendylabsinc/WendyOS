package authority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func validateAnnotationRecord(r SignedRecord) error {
	note, err := annotations.Validate(r.Component)
	if err != nil {
		return err
	}
	g := r.Grant.Grant
	if annotations.IsAmendment(r.Component) {
		a, e := annotations.ValidateAmendment(r.Component)
		if e != nil {
			return e
		}
		var original SignedRecord
		if DecodeStrictLimit(a.OriginalRecord, &original, 24<<10) != nil {
			return ErrDenied
		}
		raw, _ := json.Marshal(original)
		og := original.Grant.Grant
		if !bytes.Equal(raw, a.OriginalRecord) || og.Principal != g.Principal || og.Source != g.Source || og.PublishAudience != g.PublishAudience || !bytes.Equal(og.PublicKey, g.PublicKey) {
			return ErrDenied
		}
	}
	if note.Clock != nil {
		interval, e := note.Clock.Interval()
		if e != nil || interval.LowerUnixMS < g.NotBeforeUnixMS || interval.UpperUnixMS >= g.NotAfterUnixMS {
			return ErrDenied
		}
	}
	if g.Kind != "operator" || g.Domain != "simulation" || !contains(g.Scopes, "annotation-write") || g.Principal != note.AuthorPrincipal || g.Source != r.Component.Key.Source || g.Tenant != r.Component.Key.Tenant || g.Site != r.Component.Key.Site || r.SignedAtUnixMS != note.AuthoredAtUnixMS || r.ExpiresAtUnixMS != note.AuthoredAtUnixMS+annotations.MaxTTL.Milliseconds() {
		return ErrDenied
	}
	return nil
}

// SignAnnotation freezes issuance at draft creation. Retry the resulting record
// unchanged, with a fresh transport request nonce. It never renews note expiry.
func SignAnnotation(g SignedGrant, key ed25519.PrivateKey, c world.Component) (SignedRecord, error) {
	n, err := annotations.Validate(c)
	if err != nil {
		return SignedRecord{}, err
	}
	r, err := SignRecord(g, key, c, time.UnixMilli(n.AuthoredAtUnixMS))
	if err != nil {
		return SignedRecord{}, err
	}
	if err = validateGrant(g.Grant, 365*24*time.Hour); err != nil {
		return SignedRecord{}, err
	}
	if err = validateAnnotationRecord(r); err != nil {
		return SignedRecord{}, err
	}
	if r.SignedAtUnixMS < g.Grant.NotBeforeUnixMS || r.SignedAtUnixMS >= g.Grant.NotAfterUnixMS {
		return SignedRecord{}, ErrDenied
	}
	return r, nil
}

// VerifyAnnotationHistory checks immutable origin evidence when reopening local
// storage. It does not authorize visibility or new admission: expired/revoked
// records remain high-water entries. Live paths must also call VerifyRecord.
func (v *Verifier) VerifyAnnotationHistory(r SignedRecord) error {
	v.lock()
	defer v.mu.Unlock()
	return v.verifyAnnotationHistory(r)
}
func (v *Verifier) verifyAnnotationHistory(r SignedRecord) error {
	g := r.Grant.Grant
	if err := v.annotationAmendmentProof(r); err != nil {
		return err
	}
	if err := v.annotationClockRecord(r, time.Time{}, true); err != nil {
		return err
	}
	if err := validateAnnotationRecord(r); err != nil {
		return err
	}
	if r.Version != Version || !validWindow(r.SignedAtUnixMS, r.ExpiresAtUnixMS, v.cfg.MaxRecordLifetime) || validateGrant(g, v.cfg.MaxGrantLifetime) != nil || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != v.cfg.Domain || r.SignedAtUnixMS < g.NotBeforeUnixMS || r.SignedAtUnixMS >= g.NotAfterUnixMS || !verify(v.cfg.RootPublicKey, "grant", g, r.Grant.Signature) || !verify(g.PublicKey, "record", recordPayload(r), r.Signature) {
		return ErrDenied
	}
	return nil
}

// Called under verifier lock. Embedded ancestors establish immutable history,
// not current authorization or historical revocation knowledge.
func (v *Verifier) annotationAmendmentProof(r SignedRecord) error {
	if !annotations.IsAmendment(r.Component) {
		return nil
	}
	a, e := annotations.ValidateAmendment(r.Component)
	if e != nil {
		return e
	}
	var original SignedRecord
	if DecodeStrictLimit(a.OriginalRecord, &original, 24<<10) != nil {
		return ErrDenied
	}
	if e = v.verifyAnnotationHistory(original); e != nil {
		return e
	}
	if len(a.PredecessorRecord) > 0 {
		var prior SignedRecord
		if DecodeStrictLimit(a.PredecessorRecord, &prior, 48<<10) != nil {
			return ErrDenied
		}
		raw, _ := json.Marshal(prior)
		if !bytes.Equal(raw, a.PredecessorRecord) {
			return ErrDenied
		}
		if e = v.verifyAnnotationHistory(prior); e != nil {
			return e
		}
	}
	return nil
}

func (v *Verifier) annotationDeadline(r SignedRecord) time.Time {
	deadline := time.UnixMilli(r.ExpiresAtUnixMS)
	if v.rev != nil {
		deadline = minTime(deadline, time.UnixMilli(v.rev.Revocations.ExpiresAtUnixMS))
	}
	deadline = minTime(deadline, r.Component.Timing.SampleTime.Add(r.Component.TTL).Add(-r.Component.Timing.ErrorBound))
	if annotations.IsAmendment(r.Component) {
		deadline = minTime(deadline, time.UnixMilli(r.Grant.Grant.NotAfterUnixMS))
	}
	return deadline
}
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
