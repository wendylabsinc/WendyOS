// Package proposals retains non-executable suggestions for operator review.
// Nothing in this package admits missions, signs operator instructions or calls
// an executor. Parameters remain untrusted, uninterpreted proposal content.
package proposals

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	Version            = 1
	MaxParametersBytes = 16 << 10
	MaxRecordBytes     = 48 << 10
	MaxPageBytes       = 64 << 10
	MaxLifetime        = 24 * time.Hour
)

var (
	ErrInvalid     = errors.New("invalid proposal")
	ErrScope       = errors.New("proposal outside configured scope or catalog")
	ErrConflict    = errors.New("proposal ID already belongs to different content or source")
	ErrCapacity    = errors.New("proposal capacity exhausted; retained IDs cannot be reused")
	ErrNotFound    = errors.New("proposal not found")
	ErrUnavailable = errors.New("proposal storage unavailable")
	ErrAuthority   = errors.New("proposal authority or request lifetime expired")
)

type Request struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	Tenant         string          `json:"tenant"`
	Site           string          `json:"site"`
	RunID          string          `json:"run_id"`
	Assignee       string          `json:"assignee"`
	Type           string          `json:"type"`
	Parameters     json.RawMessage `json:"parameters"`
	Summary        string          `json:"summary"`
	NotAfterUnixMS int64           `json:"not_after_unix_ms"`
}

// Record retains the original short-lived signed request as historical proof.
// Body is base64 JSON bytes so re-encoding cannot change its signed digest.
type Record struct {
	Sequence         uint64                  `json:"sequence"`
	Proposal         Request                 `json:"proposal"`
	Principal        string                  `json:"principal"`
	Kind             string                  `json:"kind"`
	Source           world.Source            `json:"source"`
	GrantID          string                  `json:"grant_id"`
	Class            mission.SafetyClass     `json:"class"`
	AcceptedAtUnixMS int64                   `json:"accepted_at_unix_ms"`
	BodySHA256       string                  `json:"body_sha256"`
	Body             []byte                  `json:"body"`
	Proof            authority.SignedRequest `json:"proof"`
}
type View struct {
	Record           Record `json:"record"`
	Expired          bool   `json:"expired"`
	AdapterValidated bool   `json:"adapter_validated"`
}
type Summary struct {
	ID               string              `json:"id"`
	Type             string              `json:"type"`
	Assignee         string              `json:"assignee"`
	RunID            string              `json:"run_id"`
	Principal        string              `json:"principal"`
	Kind             string              `json:"kind"`
	Source           world.Source        `json:"source"`
	Class            mission.SafetyClass `json:"class"`
	Summary          string              `json:"summary"`
	AcceptedAtUnixMS int64               `json:"accepted_at_unix_ms"`
	NotAfterUnixMS   int64               `json:"not_after_unix_ms"`
	Expired          bool                `json:"expired"`
}
type Page struct {
	Proposals []Summary `json:"proposals"`
	Next      string    `json:"next"`
}
type Config struct {
	Tenant, Site, RunID, Audience, Assignee string
	Catalog                                 map[string]mission.SafetyClass
	Now                                     func() time.Time
	MaxEntries, MaxBytes                    int
}

// ValidID is deliberately path-safe; IDs are never interpreted as filenames.
func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
