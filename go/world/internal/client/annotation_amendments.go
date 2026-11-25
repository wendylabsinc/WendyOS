package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// PrepareAnnotationAmendment preserves exact original and predecessor proof
// bytes. Persist the returned body before PublishAnnotation; retries are exact.
func (c *Client) PrepareAnnotationAmendment(original, predecessor []byte, action, text string, position [3]int64, epoch, sequence uint64, clock annotationclock.Evidence) ([]byte, error) {
	var old authority.SignedRecord
	if authority.DecodeStrictLimit(original, &old, 24<<10) != nil {
		return nil, errors.New("invalid original note")
	}
	canonical, _ := json.Marshal(old)
	if string(canonical) != string(original) {
		return nil, errors.New("original note must retain canonical signed bytes")
	}
	component, n, e := annotations.Original(annotations.Amendment{OriginalRecord: original})
	if e != nil {
		return nil, e
	}
	interval, e := clock.Interval()
	if e != nil {
		return nil, e
	}
	if e = c.validateClockComponent(annotations.Note{Clock: &clock}); e != nil {
		return nil, e
	}
	if action == "withdraw" {
		text = ""
		position = n.PositionMM
	}
	a := annotations.Amendment{OriginalRecord: original, OriginalSHA256: annotations.SHA256(original), PredecessorSHA256: annotations.SHA256(original), Action: action, AuthorPrincipal: c.config.Grant.Grant.Principal, Text: text, PositionMM: position, AuthoredAtUnixMS: interval.LowerUnixMS, ExpiresAtUnixMS: n.ExpiresAtUnixMS, Clock: &clock}
	if len(predecessor) > 0 {
		var prior authority.SignedRecord
		if authority.DecodeStrictLimit(predecessor, &prior, 48<<10) != nil {
			return nil, errors.New("invalid predecessor")
		}
		a.PredecessorRecord = predecessor
		a.PredecessorSHA256 = annotations.SHA256(prior.Component.Payload)
	}
	if interval.UpperUnixMS >= a.ExpiresAtUnixMS {
		return nil, errors.New("original note expired")
	}
	amended, e := annotations.BuildAmendment(a, annotations.Config{Tenant: c.config.Grant.Grant.Tenant, Site: c.config.Grant.Grant.Site, RunID: component.Provenance.Simulation.RunID, Source: c.config.Grant.Grant.Source, Frame: component.Frame, Epoch: epoch, Sequence: sequence})
	if e != nil {
		return nil, e
	}
	record, e := authority.SignAnnotation(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), amended)
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(record)
	if len(raw) > authority.MaxEnvelopeBytes {
		return nil, errors.New("complete signed amendment exceeds envelope bound")
	}
	return raw, e
}

type AnnotationView struct {
	OriginalSHA256          string          `json:"original_sha256"`
	OriginalKey             json.RawMessage `json:"original_key"`
	Frame                   json.RawMessage `json:"frame"`
	AuthorPrincipal         string          `json:"author_principal"`
	State                   string          `json:"state"`
	Text                    string          `json:"text"`
	PositionMM              [3]int64        `json:"position_mm"`
	HeadSHA256              string          `json:"head_sha256"`
	ExpiresAtUnixMS         int64           `json:"expires_at_unix_ms"`
	AmendmentSHA256         []string        `json:"amendment_sha256"`
	Conflict                bool            `json:"conflict"`
	VerificationUntilUnixMS int64           `json:"verification_until_unix_ms"`
	CurrentVerification     bool            `json:"current_verification"`
}
type AnnotationViewsPage struct {
	Views      []AnnotationView `json:"views"`
	Revision   string           `json:"revision"`
	NextOffset int              `json:"next_offset"`
	Complete   bool             `json:"complete"`
	ObservedAt time.Time        `json:"observed_at"`
	Scope      string           `json:"scope"`
}

func (c *Client) AnnotationViews(ctx context.Context) ([]AnnotationView, error) {
	result := []AnnotationView{}
	revision := ""
	offset := 0
	for pages := 0; pages < 512; pages++ {
		body, _ := json.Marshal(struct {
			Revision string `json:"revision"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}{revision, offset, 8})
		var p AnnotationViewsPage
		if e := c.Do(ctx, "read", http.MethodPost, "/v1/annotations/views", body, &p); e != nil {
			return nil, e
		}
		if !annotations.DigestValid(p.Revision) || revision != "" && p.Revision != revision || p.NextOffset != offset+len(p.Views) || len(p.Views) > 8 || !p.Complete && len(p.Views) == 0 {
			return nil, errors.New("invalid annotation view page")
		}
		for _, v := range p.Views {
			var key struct{ Tenant, Site string }
			if json.Unmarshal(v.OriginalKey, &key) != nil || key.Tenant != c.config.Grant.Grant.Tenant || key.Site != c.config.Grant.Grant.Site || !annotations.DigestValid(v.OriginalSHA256) || !annotations.DigestValid(v.HeadSHA256) {
				return nil, errors.New("annotation view scope mismatch")
			}
		}
		result = append(result, p.Views...)
		if p.Complete {
			return result, nil
		}
		offset = p.NextOffset
		revision = p.Revision
	}
	return nil, errors.New("annotation view traversal exceeds bound; no truncated view returned")
}
