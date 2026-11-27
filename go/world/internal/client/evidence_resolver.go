package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/evidencecache"
	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

type EvidenceReference struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

func (r EvidenceReference) Validate() error {
	hash, err := hex.DecodeString(r.SHA256)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != r.SHA256 || r.Size < 1 || r.Size > 64<<20 || !packagecache.ValidContentType(r.ContentType) {
		return errors.New("exact bounded evidence digest, size and MIME reference required")
	}
	return nil
}

type EvidenceSource struct {
	ID     string
	Client *Client
}
type EvidenceResolveOptions struct {
	Sources    []EvidenceSource
	CacheDir   string
	CacheBytes int64
}
type EvidenceAttempt struct {
	Source     string `json:"source"`
	Outcome    string `json:"outcome"`
	HTTPStatus int    `json:"http_status,omitempty"`
	packagecache.Progress
}
type EvidenceResolution struct {
	Status            string            `json:"status"`
	Reference         EvidenceReference `json:"reference"`
	Source            string            `json:"source,omitempty"`
	RetainUntilUnixMS int64             `json:"retain_until_unix_ms,omitempty"`
	Attempts          []EvidenceAttempt `json:"attempts"`
	Data              []byte            `json:"-"`
}

var ErrEvidenceUnresolved = errors.New("evidence was not retrieved from the configured sources; inspect per-source outcomes")
var errEvidenceResponse = errors.New("evidence response is inconsistent with the reference or expired")

// ResolveEvidence tries only explicit authorized clients. No remote URL, digest,
// cache hit or prior success supplies authority. Every source has an independent
// checkpoint; a successful final probe rechecks access after local persistence.
func ResolveEvidence(ctx context.Context, ref EvidenceReference, opts EvidenceResolveOptions) (EvidenceResolution, error) {
	out := EvidenceResolution{Status: "unavailable", Reference: ref, Attempts: []EvidenceAttempt{}}
	if err := ref.Validate(); err != nil {
		return out, err
	}
	if len(opts.Sources) < 1 || len(opts.Sources) > 8 {
		return out, errors.New("one to eight explicit evidence sources required")
	}
	cfg := evidencecache.Config{MaxBytes: opts.CacheBytes}
	for _, s := range opts.Sources {
		if s.Client == nil {
			return out, errors.New("evidence client required")
		}
		c := s.Client.config
		g := c.Grant.Grant
		hasRead := false
		for _, scope := range g.Scopes {
			if scope == "evidence-read" {
				hasRead = true
			}
		}
		if g.Domain != "simulation" || !hasRead {
			return out, errors.New("each source requires explicit simulation evidence-read credentials")
		}
		cfg.Sources = append(cfg.Sources, evidencecache.Source{ID: s.ID, Scope: packagecache.Scope{Tenant: g.Tenant, Site: g.Site, Domain: g.Domain, Audience: c.Audience, Endpoint: c.URL}})
	}
	cache, err := evidencecache.Open(opts.CacheDir, cfg)
	if err != nil {
		return out, err
	}
	defer cache.Close()
	for _, s := range opts.Sources {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		attempt := EvidenceAttempt{Source: s.ID}
		source, openErr := cache.Source(s.ID)
		if openErr != nil {
			attempt.Outcome = "cache_error"
			out.Attempts = append(out.Attempts, attempt)
			continue
		}
		step, cancel := context.WithTimeout(ctx, 15*time.Second)
		var metadata evidence.GetResponse
		get := func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
			got, e := s.Client.GetEvidence(ctx, r)
			if e != nil {
				return got, e
			}
			if got.SHA256 != ref.SHA256 || got.Size != ref.Size || got.ContentType != ref.ContentType || got.Offset != r.Offset || len(got.Data) < 1 || len(got.Data) > r.Length || int64(len(got.Data)) > ref.Size-r.Offset || got.RetainUntilUnixMS <= time.Now().UnixMilli() {
				return got, errEvidenceResponse
			}
			metadata = got
			metadata.Data = nil
			return got, nil
		}
		data, progress, fetchErr := source.FetchEvidence(step, ref.SHA256, ref.Size, ref.ContentType, get)
		attempt.Progress = progress
		if fetchErr == nil {
			before := metadata
			var probe evidence.GetResponse
			probe, fetchErr = get(step, evidence.GetRequest{SHA256: ref.SHA256, Offset: 0, Length: 1})
			if fetchErr == nil {
				attempt.DownloadedBytes++
				sum := sha256.Sum256(data)
				if probe.RetainUntilUnixMS != before.RetainUntilUnixMS || probe.Data[0] != data[0] || int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
					fetchErr = errEvidenceResponse
				}
			}
		}
		if fetchErr == nil {
			fetchErr = step.Err()
		}
		cancel()
		source.Close()
		if fetchErr != nil {
			attempt.Outcome = "invalid"
			var status *HTTPStatusError
			var network net.Error
			switch {
			case errors.As(fetchErr, &status):
				attempt.HTTPStatus = status.StatusCode
				switch {
				case status.StatusCode == 401 || status.StatusCode == 403:
					attempt.Outcome = "denied"
				case status.StatusCode == 404:
					attempt.Outcome = "not_found"
				case status.StatusCode == 410:
					attempt.Outcome = "expired"
				case status.StatusCode >= 500:
					attempt.Outcome = "unavailable"
				default:
					attempt.Outcome = "refused"
				}
			case errors.Is(fetchErr, context.DeadlineExceeded) || errors.As(fetchErr, &network):
				attempt.Outcome = "unavailable"
			case errors.Is(fetchErr, packagecache.ErrQuota):
				attempt.Outcome = "cache_quota"
			}
			out.Attempts = append(out.Attempts, attempt)
			continue
		}
		attempt.Outcome = "verified"
		out.Attempts = append(out.Attempts, attempt)
		out.Status = "verified"
		out.Source = s.ID
		out.RetainUntilUnixMS = metadata.RetainUntilUnixMS
		out.Data = data
		return out, nil
	}
	for _, a := range out.Attempts {
		if a.Outcome == "denied" {
			out.Status = "denied"
			break
		}
	}
	return out, ErrEvidenceUnresolved
}
