package livefusion

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// This marker exists only during one persistent-session selection. It records
// a successful, fully parsed current Query that omitted a selected source;
// it does not turn an authorization refusal into a verified input.
type captureReadKey struct{}
type captureReadState struct {
	missing map[string]bool
	batch   serviceorigin.BatchAuthorization
}

// expiredSelection permits a later acquisition tick only when every missing
// original has expired. Signature, scope, RPC and clock failures remain errors.
// No input from the refused batch may be used for evaluation or publication.
func (s *session) expiredSelection(ctx context.Context, proofs []inputProof, failure error) (string, error) {
	read, _ := ctx.Value(captureReadKey{}).(*captureReadState)
	if read == nil || len(read.missing) == 0 ||
		(!errors.Is(failure, serviceorigin.ErrDenied) && !errors.Is(failure, ErrSource)) {
		return "", failure
	}
	now, err := s.clock.Observe(ctx)
	if err != nil {
		return "", err
	}
	if len(read.batch.Claims) != len(proofs) || len(read.batch.Devices) != len(proofs) {
		return "", ErrSource
	}
	for i, claims := range read.batch.Claims {
		device := read.batch.Devices[i]
		if now.Physical.Earliest.UnixMilli() < claims.IssuedAtUnixMS || now.Physical.Latest.UnixMilli() >= claims.ExpiresAtUnixMS ||
			now.Physical.Earliest.Before(device.ValidFrom) || !now.Physical.Latest.Before(device.ValidUntil) {
			return "", ErrSource
		}
	}
	reason := "sample_capture_expired"
	for _, p := range proofs {
		captured := time.Unix(0, p.capture.CapturedUnixNS)
		if p.capture.CapturedUnixNS <= 0 || captured.After(now.Physical.Latest) ||
			p.record.Publication.Timing.SampleTime.After(now.SampleTime) ||
			now.Physical.Earliest.UnixMilli() < p.record.IssuedAtUnixMS || now.Physical.Latest.UnixMilli() >= p.record.ExpiresAtUnixMS {
			return "", ErrSource
		}
		component := (serviceorigin.VerifiedRecord{Record: p.record, Bytes: p.signed.Record}).Component()
		sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component)
		if err != nil {
			return "", ErrSource
		}
		if !read.missing[p.record.Publication.Stream] {
			continue
		}
		physicalExpired := !now.Physical.Latest.Before(physicalUntil)
		if now.SampleTime.Before(sampleUntil) && !physicalExpired {
			return "", failure
		}
		if physicalExpired {
			reason = "physical_capture_expired"
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.failureCode = ""
	return reason, nil
}
