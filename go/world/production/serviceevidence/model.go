// Package serviceevidence stores source-owned media behind the private agent's
// service authorization boundary. It does not authorize callers or publish World
// records; its host must supply the immutable authenticated owner and a current
// lifecycle fence. Digests identify bytes and never grant access.
package serviceevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const MaxChunk = evidence.MaxChunk

var (
	ErrDenied      = errors.New("service evidence owner or request denied")
	ErrClock       = errors.New("authenticated evidence retention clock unavailable or regressed")
	ErrFaulted     = errors.New("service evidence store faulted; reopen with verified state required")
	ErrCorrupt     = errors.New("service evidence state corrupt or incomplete")
	ErrClosed      = errors.New("service evidence store closed")
	ErrUncertain   = errors.New("service evidence operation outcome may be unknown")
	ErrUnavailable = evidence.ErrUnavailable
	ErrQuota       = evidence.ErrQuota
)

// Owner is constructed from the commissioned caller/subscription. Device is a
// canonical enrolled-device principal. Deployment generation is intentionally
// absent: source-owned evidence survives deployment replacement and observation
// expiry, subject to current read grants and its original retention deadline.
type Owner struct {
	Tenant    string `json:"tenant"`
	Site      string `json:"site"`
	Device    string `json:"device"`
	App       string `json:"app"`
	Service   string `json:"service"`
	Component string `json:"component"`
	Stream    string `json:"stream"`
}

func (o Owner) Validate() error {
	return (serviceorigin.Scope{Tenant: o.Tenant, Site: o.Site, Device: o.Device, App: o.App, Service: o.Service, DeploymentGeneration: 1, ExecutionDomain: "physical", Publish: []serviceorigin.PublishScope{{ComponentType: o.Component, Stream: o.Stream}}}).Validate()
}
func (o Owner) SHA256() (string, error) {
	if o.Validate() != nil {
		return "", ErrDenied
	}
	b, _ := json.Marshal(struct {
		Version int   `json:"version"`
		Owner   Owner `json:"owner"`
	}{1, o})
	return digest(b), nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type Config struct {
	Directory                string
	Tenant, Site, Device     string
	Clock                    trustedtime.Provider
	QuotaBytes, MaxBlobBytes int64
	MaxEntries               int
	MaxRetention             time.Duration
}

// PutRequest contains no identity or arbitrary file path. Zero retention asks
// for the configured maximum on first allocation; retry never renews it.
type PutRequest struct {
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	Offset            int64  `json:"offset"`
	Data              []byte `json:"data"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
}
type GetRequest struct {
	SHA256 string `json:"sha256"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}
type Reference struct {
	Owner             Owner  `json:"owner"`
	OwnerSHA256       string `json:"owner_sha256"`
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	ContentType       string `json:"content_type"`
	RetainUntilUnixMS int64  `json:"retain_until_unix_ms"`
}
type PutResponse struct {
	Reference
	Received int64 `json:"received"`
	Complete bool  `json:"complete"`
}
type GetResponse struct {
	Reference
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}
type Store struct {
	mu       sync.Mutex
	cfg      Config
	inner    *evidence.Store
	lock     *os.File
	state    diskState
	observed trustedtime.Interval
	storeNow time.Time
	fault    error
	closed   bool
}
type diskState struct {
	Version             int                  `json:"version"`
	ConfigurationSHA256 string               `json:"configuration_sha256"`
	Floor               trustedtime.Interval `json:"physical_floor"`
}
type checkedState struct {
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}
