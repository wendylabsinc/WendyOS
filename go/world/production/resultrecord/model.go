// Package resultrecord validates generic derived-result envelopes. The host
// supplies verified raw input references and their effective live deadlines;
// this package neither establishes authority nor interprets domain results.
package resultrecord

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
)

const (
	Version         = 1
	ComponentType   = "wendy.inference.result.v1"
	MaxPayloadBytes = 32 << 10
	MaxResultBytes  = 24 << 10
	MaxInputs       = 16
	MaxLifetime     = 30 * 24 * time.Hour
)

// ResultJSON is base64 in the envelope so its original JSON bytes, including
// whitespace and number encodings, survive ordinary JSON marshaling. Inputs
// name immutable proof bytes and actual agent identity/version, never a caller
// selected replacement identity. Retention is a requested physical deadline;
// the host archive must authorize and commit it separately from live validity.
type Result struct {
	Version           int                  `json:"version"`
	ResultSchema      string               `json:"result_schema"`
	ResultJSON        []byte               `json:"result_json"`
	Inputs            []proofref.Reference `json:"inputs"`
	RetainUntilUnixMS int64                `json:"retain_until_unix_ms"`
}

// ValidatedInput is an in-agent trust injection seam, not an app request. The
// host must verify the exact envelope/record hashes, signature chain, current
// source and reader authority, schema, source clock and media before supplying
// it. SampleValidUntil includes source uncertainty, capture interval/calibration
// and any stricter local bound. PhysicalValidUntil includes signing authority
// and physical capture age. Neither deadline may be omitted.
type ValidatedInput struct {
	Reference          proofref.Reference
	Component          world.Component
	SampleValidUntil   time.Time
	PhysicalValidUntil time.Time
}

// Validation separates live output deadlines from requested historical
// retention. PhysicalValidUntil is the earliest host-supplied input bound and
// must be checked again against authenticated physical time at admission and
// after blocking work. SampleValidUntil is the output's conservative effective
// deadline. AgeHint can shorten remaining validity further at the replica.
type Validation struct {
	Result             Result
	SampleValidUntil   time.Time
	PhysicalValidUntil time.Time
}
