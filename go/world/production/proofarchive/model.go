// Package proofarchive retains exact host-admitted proofs for bounded audit
// and claim dependencies. Historical reads never restore live replica state.
package proofarchive

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

type Reference = proofref.Reference
type Source = proofref.Source

var (
	ErrDenied         = errors.New("proof archive scope or authority refused")
	ErrUnavailable    = errors.New("exact retained proof or pin unavailable")
	ErrConflict       = errors.New("proof version or immutable pin conflict")
	ErrQuota          = errors.New("proof archive logical quota exceeded")
	ErrClock          = errors.New("proof archive physical clock unavailable or regressed")
	ErrUncertain      = errors.New("proof archive commit outcome uncertain; reopen required")
	ErrFaulted        = errors.New("proof archive faulted; reopen required")
	ErrCorrupt        = errors.New("proof archive integrity failure")
	ErrNotInitialized = errors.New("proof archive is not initialized")
	ErrClosed         = errors.New("proof archive closed")
)

type Config struct {
	Path                 string
	Tenant, Site, Device string
	Clock                trustedtime.Provider
	Verifier             *serviceorigin.Verifier
	// RemoteVerifier and ValidateRemoteAccepted are an optional pair for exact
	// foreign camera/audio/gas/LiDAR capture envelopes admitted by the host. The
	// callback binds owned envelope bytes to an operation-local remote journal
	// snapshot and checks current authority, schema and original deadlines.
	// It runs under the archive lock, sometimes inside its write transaction:
	// it must not reenter journal/archive or acquire an earlier host fence.
	// The host holds its identity/remote-operation fence for the entire call.
	RemoteVerifier         *serviceorigin.Verifier
	ValidateRemoteAccepted func(context.Context, serviceorigin.VerifiedRecord, []byte) error
	// ValidatePin must bind the signed claim to exactly these inputs, verify
	// their current authority/read grants and original-clock freshness, and
	// enforce claim semantics/retention policy. It must not reenter this store.
	// Each Descriptor owns its original envelope bytes. The callback may not
	// infer current validity from historical admission or storage retention.
	ValidatePin                        func(context.Context, serviceorigin.VerifiedRecord, []Descriptor, time.Time) error
	RecentWindow, MaxRetention         time.Duration
	QuotaBytes, MaxEnvelopeBytes       int64
	MaxEntries, MaxPins, MaxRefsPerPin int
}

type Descriptor struct {
	Reference         Reference            `json:"reference"`
	Envelope          []byte               `json:"envelope"`
	Admitted          trustedtime.Interval `json:"admitted"`
	RecentUntilUnixMS int64                `json:"recent_until_unix_ms"`
	RetainUntilUnixMS int64                `json:"retain_until_unix_ms"`
}
type HistoricalProof struct {
	Descriptor
	Historical bool `json:"historical"`
}
type PinRequest struct {
	ClaimEnvelope     []byte
	Inputs            []Reference
	RetainUntilUnixMS int64
}
type PinReceipt struct {
	Claim             Reference   `json:"claim"`
	Inputs            []Reference `json:"inputs"`
	RetainUntilUnixMS int64       `json:"retain_until_unix_ms"`
}
type storedPin struct {
	Claim             Reference            `json:"claim"`
	Inputs            []Reference          `json:"inputs"`
	RetainUntilUnixMS int64                `json:"retain_until_unix_ms"`
	Created           trustedtime.Interval `json:"created"`
}
type marker struct {
	Version             int    `json:"version"`
	InstallationID      string `json:"installation_id"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}
type checked struct {
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}
type usage struct {
	Bytes        int64 `json:"bytes"`
	Entries      int   `json:"entries"`
	Pins         int   `json:"pins"`
	RememberKeys int   `json:"remember_keys"`
	PinKeys      int   `json:"pin_keys"`
}
type Archive struct {
	mu              sync.Mutex
	cfg             Config
	db              *bolt.DB
	floor, observed trustedtime.Interval
	closed          bool
	fault           error
	commit          func(func(*bolt.Tx) error) error
}

var (
	metaBucket     = []byte("metadata")
	proofBucket    = []byte("proofs")
	pinBucket      = []byte("pins")
	rememberBucket = []byte("remember-high-water")
	pinWaterBucket = []byte("pin-high-water")
	markerKey      = []byte("installation")
	floorKey       = []byte("physical-floor")
	usageKey       = []byte("usage")
)
