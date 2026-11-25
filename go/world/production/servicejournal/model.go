// Package servicejournal durably admits original service publications. Local
// publishers and remote replication use separate protected journals and
// authority contracts. It does not re-sign observations or authorize missions.
package servicejournal

import (
	"context"
	"errors"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotInitialized = errors.New("service journal is not initialized")
	ErrCorrupt        = errors.New("service journal integrity failure")
	ErrCapacity       = errors.New("service journal quota exceeded")
	ErrEpoch          = errors.New("publication does not use the currently reserved source epoch")
	ErrClosed         = errors.New("service journal closed")
	ErrFaulted        = errors.New("service journal outcome uncertain; reopen required")
)

type Config struct {
	Path          string
	Replica       world.Config
	Verifier      *serviceorigin.Verifier
	PhysicalClock trustedtime.Provider
	AdmitSchema   func(context.Context, serviceorigin.VerifiedRecord) error
	// MaxProofBytes bounds the total canonical envelopes retained, including
	// expired/tombstoned high-water entries. No automatic eviction is allowed.
	MaxProofBytes int64
	MaxEntries    int
}

type marker struct {
	Version             int    `json:"version"`
	InstallationID      string `json:"installation_id"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}
type checkedBytes struct {
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}
type storedProof struct {
	SHA256   string `json:"sha256"`
	Envelope []byte `json:"envelope"`
}
type storedEpoch struct {
	Epoch  uint64 `json:"epoch"`
	SHA256 string `json:"sha256"`
}
type source struct {
	Tenant  string `json:"tenant"`
	Device  string `json:"device"`
	App     string `json:"app"`
	Service string `json:"service"`
}

type Journal struct {
	mu              sync.Mutex
	cfg             Config
	db              *bolt.DB
	replica         *world.Replica
	proofs          map[string]storedProof
	epochs          map[string]uint64
	proofBytes      int64
	floor           trustedtime.Interval
	observed        trustedtime.Interval
	closed, faulted bool
	// remoteDevice is set only by the purpose-separated RemoteJournal opener.
	// Normal Create/Open leave it empty and accept only local journal markers.
	remoteDevice string
	// commit is replaceable by package tests to exercise ambiguous fsync
	// outcomes. Production always uses the database's synchronous Update.
	commit func(func(*bolt.Tx) error) error
}

var metaBucket = []byte("metadata")
var proofsBucket = []byte("proofs")
var epochsBucket = []byte("epochs")
var markerKey = []byte("installation")
var checkpointKey = []byte("checkpoint")
var floorKey = []byte("physical-floor")

const maxEnvelopeBytes = 384 << 10
