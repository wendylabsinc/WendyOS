package authority

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"testing"
	"time"
)

func signedAmendment(t *testing.T, f *fixture, original SignedRecord, g SignedGrant) SignedRecord {
	t.Helper()
	raw, _ := json.Marshal(original)
	_, note, _ := annotations.Original(annotations.Amendment{OriginalRecord: raw})
	a := annotations.Amendment{OriginalRecord: raw, OriginalSHA256: annotations.SHA256(raw), PredecessorSHA256: annotations.SHA256(raw), Action: "withdraw", AuthorPrincipal: g.Grant.Principal, PositionMM: note.PositionMM, AuthoredAtUnixMS: f.now.UnixMilli(), ExpiresAtUnixMS: note.ExpiresAtUnixMS}
	c, e := annotations.BuildAmendment(a, annotations.Config{Tenant: g.Grant.Tenant, Site: g.Grant.Site, RunID: "run", Source: g.Grant.Source, Frame: original.Component.Frame, Epoch: 1, Sequence: 2})
	if e != nil {
		t.Fatal(e)
	}
	r, e := SignAnnotation(g, f.key, c)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestAmendmentCurrentGrantExpiryDoesNotInvalidateHistoricalProof(t *testing.T) {
	f, original := annotationFixture(t)
	g := f.grant.Grant
	g.ID = "amendment-short"
	g.NotAfterUnixMS = f.now.Add(100 * time.Millisecond).UnixMilli()
	short, e := SignGrant(g, f.root)
	if e != nil {
		t.Fatal(e)
	}
	r := signedAmendment(t, f, original, short)
	if _, e = f.verifier.VerifyRecord(r); e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(200 * time.Millisecond)
	if _, e = f.verifier.VerifyRecord(r); e == nil {
		t.Fatal("expired current amendment grant accepted")
	}
	if e = f.verifier.VerifyAnnotationHistory(r); e != nil {
		t.Fatal("immutable historical proof lost", e)
	}
}
func TestAmendmentFinalGrantCheckAfterProofWorkAndBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			f, original := annotationFixture(t)
			g := f.grant.Grant
			g.NotAfterUnixMS = f.now.Add(100 * time.Millisecond).UnixMilli()
			short, e := SignGrant(g, f.root)
			if e != nil {
				t.Fatal(e)
			}
			r := signedAmendment(t, f, original, short)
			first := f.now
			calls := 0
			f.verifier.cfg.Now = func() time.Time {
				calls++
				if calls == 1 {
					return first
				}
				return first.Add(200 * time.Millisecond)
			}
			if batch {
				_, e = f.verifier.VerifyRecords([]SignedRecord{r, original})
			} else {
				_, e = f.verifier.VerifyRecord(r)
			}
			if e == nil {
				t.Fatal("authority expired during proof/persistence work")
			}
		})
	}
}
func TestAmendmentOriginalProofSignatureAndCurrentRevocationIndependent(t *testing.T) {
	f, original := annotationFixture(t)
	r := signedAmendment(t, f, original, f.grant)
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{f.grant.Grant.ID}
	signed, e := SignRevocations(rev, f.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.verifier.UpdateRevocations(signed); e != nil {
		t.Fatal(e)
	}
	if _, e = f.verifier.VerifyRecord(r); e == nil {
		t.Fatal("revoked author admitted")
	}
	if e = f.verifier.VerifyAnnotationHistory(r); e != nil {
		t.Fatal(e)
	}
}
