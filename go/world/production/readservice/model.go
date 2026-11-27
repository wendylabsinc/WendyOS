// Package readservice exposes a bounded read-only HTTP handler. TLS listener
// ownership, verified physical time, and backend commissioning belong to its host.
package readservice

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/authorizer"
)

const (
	Schema            = "wendy.world.query-result.v1"
	MaxComponents     = 256
	MaxComponentBytes = 64 << 10
	// Proof up to 128 KiB plus base64 of a 64 KiB body and JSON framing.
	MaxEnvelopeBytes = (128 << 10) + ((64<<10)+2)/3*4 + 256
)

type FrameRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type Dataset struct {
	ID           string   `json:"id"`
	Domain       string   `json:"domain"`
	SourceTenant string   `json:"source_tenant"`
	SourceSite   string   `json:"source_site"`
	Frame        FrameRef `json:"frame"`
	RunID        string   `json:"run_id"`
}
type Query struct {
	Version   int    `json:"version"`
	DatasetID string `json:"dataset_id"`
	Limit     int    `json:"limit"`
}
type Envelope struct {
	Proof      string `json:"proof"`
	BodyBase64 string `json:"body_base64"`
}
type Snapshot struct {
	Dataset    Dataset           `json:"dataset"`
	Cursor     string            `json:"cursor"`
	Components []json.RawMessage `json:"components"`
}

// Reader is a locally commissioned adapter, never a caller-selected URL/file.
// It must honor cancellation, return owned immutable records, enforce source
// scope/replica expiry, and preserve original provenance. It must not claim that
// a configured simulation gateway is a production measurement authority.
type Reader interface {
	Query(context.Context, Query) (Snapshot, error)
}

type Config struct {
	Tenant, Site, Audience                           string
	Dataset                                          Dataset
	Authorizer                                       *authorizer.Authorizer
	Reader                                           Reader
	MaxConcurrent, MaxPerPrincipal, MaxResponseBytes int
	RequestTimeout                                   time.Duration
}
type AuthorityScope struct {
	Domain   string `json:"domain"`
	Tenant   string `json:"tenant"`
	Site     string `json:"site"`
	Audience string `json:"audience"`
}
type Response struct {
	Schema     string            `json:"schema"`
	Authority  AuthorityScope    `json:"authority"`
	Dataset    Dataset           `json:"dataset"`
	Cursor     string            `json:"cursor"`
	Components []json.RawMessage `json:"components"`
}
