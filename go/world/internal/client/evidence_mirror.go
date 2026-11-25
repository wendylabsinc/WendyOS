package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencemirror"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

func mirrorEndpoint(c *Client, scopes ...string) (evidencemirror.Endpoint, error) {
	if c == nil {
		return evidencemirror.Endpoint{}, evidencemirror.ErrInvalid
	}
	g := c.config.Grant.Grant
	for _, want := range scopes {
		if want == "evidence-write" && g.PublishAudience != c.config.Audience {
			return evidencemirror.Endpoint{}, errors.New("destination-bound evidence-write authority required")
		}
		found := false
		for _, s := range g.Scopes {
			if s == want {
				found = true
			}
		}
		if !found {
			return evidencemirror.Endpoint{}, errors.New("explicit evidence scope required")
		}
	}
	sum := sha256.Sum256(g.PublicKey)
	e := evidencemirror.Endpoint{Tenant: g.Tenant, Site: g.Site, Domain: g.Domain, Audience: c.config.Audience, URL: c.config.URL, Principal: g.Principal, KeySHA256: hex.EncodeToString(sum[:])}
	return e, e.Validate()
}
func mirrorRead(ctx context.Context, c *Client, p evidencemirror.Plan) (evidence.GetResponse, error) {
	q := evidence.GetRequest{SHA256: p.Reference.SHA256, Length: 1}
	r, e := c.GetEvidence(ctx, q)
	if e != nil {
		return r, e
	}
	if r.SHA256 != p.Reference.SHA256 || r.Size != p.Reference.Size || r.ContentType != p.Reference.ContentType || r.RetainUntilUnixMS != p.RetainUntilUnixMS || r.Offset != 0 || len(r.Data) != 1 {
		return r, evidencemirror.ErrChanged
	}
	if time.Now().UnixMilli() >= p.RetainUntilUnixMS {
		return r, evidencemirror.ErrExpired
	}
	return r, ctx.Err()
}

// PrepareEvidenceMirror records the original source metadata before any remote
// write. It does not infer that the serving source is the original capture owner.
func PrepareEvidenceMirror(ctx context.Context, id, path string, source, destination *Client, ref EvidenceReference) (evidencemirror.State, error) {
	var zero evidencemirror.State
	if e := ref.Validate(); e != nil {
		return zero, e
	}
	src, e := mirrorEndpoint(source, "evidence-read")
	if e != nil {
		return zero, e
	}
	dst, e := mirrorEndpoint(destination, "evidence-write", "evidence-read")
	if e != nil {
		return zero, e
	}
	if src.Tenant != dst.Tenant || src.Site != dst.Site || src.URL == dst.URL {
		return zero, evidencemirror.ErrChanged
	}
	metadata, e := source.GetEvidence(ctx, evidence.GetRequest{SHA256: ref.SHA256, Length: 1})
	if e != nil {
		return zero, e
	}
	if metadata.SHA256 != ref.SHA256 || metadata.Size != ref.Size || metadata.ContentType != ref.ContentType || metadata.Offset != 0 || len(metadata.Data) != 1 {
		return zero, evidencemirror.ErrChanged
	}
	now := time.Now()
	p := evidencemirror.Plan{Version: 1, ID: id, Source: src, Destination: dst, Reference: evidencemirror.Reference{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType}, RetainUntilUnixMS: metadata.RetainUntilUnixMS, PreparedUnixMS: now.UnixMilli()}
	if e = ctx.Err(); e != nil {
		return zero, e
	}
	if p.Validate() != nil || now.UnixMilli() >= p.RetainUntilUnixMS {
		return zero, evidencemirror.ErrInvalid
	}
	j, e := evidencemirror.Create(path, p)
	if e != nil {
		return zero, e
	}
	defer j.Close()
	return j.Snapshot(), nil
}
func mirrorFailure(e error) string {
	var status *HTTPStatusError
	switch {
	case errors.Is(e, evidencemirror.ErrExpired):
		return "expired"
	case errors.Is(e, evidencemirror.ErrChanged):
		return "metadata_or_scope_changed"
	case errors.As(e, &status):
		switch status.StatusCode {
		case 401, 403:
			return "denied"
		case 404:
			return "unavailable"
		case 507:
			return "quota"
		case 409:
			return "conflict"
		}
		return "http_failure"
	case errors.Is(e, context.DeadlineExceeded), errors.Is(e, context.Canceled):
		return "interrupted"
	}
	return "unavailable_or_invalid"
}
func mirrorUpdate(j *evidencemirror.Journal, phase string, change func(*evidencemirror.State)) error {
	s := j.Snapshot()
	s.Phase = phase
	s.LastError = ""
	if change != nil {
		change(&s)
	}
	if e := j.Update(s, time.Now()); e != nil {
		return e
	}
	if j.Snapshot().Phase == "expired" {
		return evidencemirror.ErrExpired
	}
	return nil
}
func mirrorOutcome(j *evidencemirror.Journal, err error) (evidencemirror.Status, error) {
	if err != nil {
		s := j.Snapshot()
		s.Phase = "blocked"
		s.LastError = mirrorFailure(err)
		if e := j.Update(s, time.Now()); e != nil {
			return evidencemirror.Status{State: j.Snapshot(), Availability: "journal_unavailable"}, e
		}
	}
	out, e := j.Status(time.Now())
	if e != nil {
		return out, e
	}
	return out, err
}
func validateMirrorPut(r evidence.PutResponse, p evidencemirror.Plan, minimum int64) error {
	if r.SHA256 != p.Reference.SHA256 || r.Size != p.Reference.Size || r.ContentType != p.Reference.ContentType || r.RetainUntilUnixMS != p.RetainUntilUnixMS || r.Received < minimum || r.Received > p.Reference.Size || r.Complete != (r.Received == p.Reference.Size) {
		return evidencemirror.ErrChanged
	}
	if time.Now().UnixMilli() >= p.RetainUntilUnixMS {
		return evidencemirror.ErrExpired
	}
	return nil
}
func verifyMirror(ctx context.Context, j *evidencemirror.Journal, destination *Client) error {
	p := j.Snapshot().Plan
	if e := mirrorUpdate(j, "verifying", nil); e != nil {
		return e
	}
	data, meta, e := destination.DownloadEvidence(ctx, p.Reference.SHA256, p.Reference.Size)
	if e != nil {
		return e
	}
	if meta.Size != p.Reference.Size || meta.ContentType != p.Reference.ContentType || meta.RetainUntilUnixMS != p.RetainUntilUnixMS {
		return evidencemirror.ErrChanged
	}
	// A fresh final authenticated range follows the full digest read. The original
	// deadline is checked again; this is an availability observation, not a pin.
	probe, e := mirrorRead(ctx, destination, p)
	if e != nil {
		return e
	}
	if len(data) != int(p.Reference.Size) || data[0] != probe.Data[0] {
		return evidencemirror.ErrChanged
	}
	if e = j.Check(); e != nil {
		return e
	}
	return mirrorUpdate(j, "verified", func(s *evidencemirror.State) {
		s.DestinationReceived = p.Reference.Size
		s.DestinationComplete = true
		s.VerifiedUnixMS = time.Now().UnixMilli()
	})
}

// RunEvidenceMirror resumes exactly one immutable plan. A current source read
// precedes each destination write; all bytes use the original source deadline.
func RunEvidenceMirror(ctx context.Context, path string, source, destination *Client) (out evidencemirror.Status, err error) {
	j, e := evidencemirror.Open(path)
	if e != nil {
		return out, e
	}
	defer j.Close()
	p := j.Snapshot().Plan
	src, e := mirrorEndpoint(source, "evidence-read")
	if e != nil {
		return mirrorOutcome(j, e)
	}
	dst, e := mirrorEndpoint(destination, "evidence-write", "evidence-read")
	if e != nil {
		return mirrorOutcome(j, e)
	}
	if src != p.Source || dst != p.Destination {
		return mirrorOutcome(j, evidencemirror.ErrChanged)
	}
	if e = mirrorUpdate(j, "downloading", func(s *evidencemirror.State) { s.Attempts++; s.SourceResumedBytes = 0; s.SourceDownloadedBytes = 0 }); e != nil {
		return mirrorOutcome(j, e)
	}
	if _, e = mirrorRead(ctx, source, p); e != nil {
		return mirrorOutcome(j, e)
	}
	cache, e := packagecache.Open(j.CacheDirectory(), packagecache.Config{Evidence: true, Scope: packagecache.Scope{Tenant: src.Tenant, Site: src.Site, Domain: src.Domain, Audience: src.Audience, Endpoint: src.URL}, MaxBytes: p.Reference.Size, MaxEntries: 1})
	if e != nil {
		return mirrorOutcome(j, e)
	}
	get := func(ctx context.Context, q evidence.GetRequest) (evidence.GetResponse, error) {
		r, e := source.GetEvidence(ctx, q)
		if e != nil {
			return r, e
		}
		if r.SHA256 != p.Reference.SHA256 || r.Size != p.Reference.Size || r.ContentType != p.Reference.ContentType || r.RetainUntilUnixMS != p.RetainUntilUnixMS {
			return r, evidencemirror.ErrChanged
		}
		return r, nil
	}
	data, progress, fetchErr := cache.FetchEvidence(ctx, p.Reference.SHA256, p.Reference.Size, p.Reference.ContentType, get)
	closeErr := cache.Close()
	e = mirrorUpdate(j, "downloading", func(s *evidencemirror.State) {
		s.SourceResumedBytes = progress.ResumedBytes
		s.SourceDownloadedBytes = progress.DownloadedBytes
	})
	if e != nil {
		return mirrorOutcome(j, e)
	}
	if fetchErr != nil {
		return mirrorOutcome(j, fetchErr)
	}
	if closeErr != nil {
		return mirrorOutcome(j, closeErr)
	}
	if e = j.Check(); e != nil {
		return mirrorOutcome(j, e)
	}
	// Persist intent before the allocation/progress probe, which can itself write.
	if e = mirrorUpdate(j, "uploading", nil); e != nil {
		return mirrorOutcome(j, e)
	}
	q := evidence.PutRequest{SHA256: p.Reference.SHA256, Size: p.Reference.Size, ContentType: p.Reference.ContentType, RetainUntilUnixMS: p.RetainUntilUnixMS}
	for {
		if _, e = mirrorRead(ctx, source, p); e != nil {
			return mirrorOutcome(j, e)
		}
		if e = j.Check(); e != nil {
			return mirrorOutcome(j, e)
		}
		response, putErr := destination.PutEvidence(ctx, q)
		if putErr != nil {
			return mirrorOutcome(j, putErr)
		}
		minimum := j.Snapshot().DestinationReceived
		if len(q.Data) > 0 && q.Offset+int64(len(q.Data)) > minimum {
			minimum = q.Offset + int64(len(q.Data))
		}
		if e = validateMirrorPut(response, p, minimum); e != nil {
			return mirrorOutcome(j, e)
		}
		if e = mirrorUpdate(j, "uploading", func(s *evidencemirror.State) {
			s.DestinationReceived = response.Received
			s.DestinationComplete = response.Complete
		}); e != nil {
			return mirrorOutcome(j, e)
		}
		if response.Complete {
			break
		}
		q.Offset = response.Received
		end := q.Offset + evidence.MaxChunk
		if end > q.Size {
			end = q.Size
		}
		q.Data = data[q.Offset:end]
	}
	return mirrorOutcome(j, verifyMirror(ctx, j, destination))
}

// VerifyEvidenceMirror uses any currently authorized reader at the exact planned
// destination. It can succeed with the source offline and does not modify blobs.
func VerifyEvidenceMirror(ctx context.Context, path string, destination *Client) (out evidencemirror.Status, err error) {
	j, e := evidencemirror.Open(path)
	if e != nil {
		return out, e
	}
	defer j.Close()
	dst, e := mirrorEndpoint(destination, "evidence-read")
	if e != nil {
		return mirrorOutcome(j, e)
	}
	want := j.Snapshot().Plan.Destination
	if dst.URL != want.URL || dst.Audience != want.Audience || dst.Tenant != want.Tenant || dst.Site != want.Site || dst.Domain != want.Domain {
		return mirrorOutcome(j, evidencemirror.ErrChanged)
	}
	return mirrorOutcome(j, verifyMirror(ctx, j, destination))
}
