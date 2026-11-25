package servicejournal

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const MaxInventoryEntries = 64

// AuthorityGuard runs fn exactly once while receiver identity/policy and remote
// lease/floor mutation are serialized by the same host-owned fence. The lock
// order is host identity -> remote fence -> journal -> sourceauthority. No
// callback may acquire an earlier lock, reenter a journal, or wait on network.
// fn's error must be propagated. A completed mutation plus a later guard error
// is uncertain, never an acknowledged rollback.
type AuthorityGuard func(context.Context, func() error) error

// AuthorityWindow is an operation-local intersection of current lease, policy
// and identity validity. It is not a cached grant. The journal checks it again
// against one fresh final interval after all blocking work.
type AuthorityWindow struct{ NotBefore, NotAfter time.Time }

type RemoteConfig struct {
	Path        string
	LocalDevice string
	Replica     world.Config
	// Verifier is a separate remote cryptographic verifier. Its mandatory
	// authorizer must not borrow local publisher/process epoch reservations.
	Verifier      *serviceorigin.Verifier
	PhysicalClock trustedtime.Provider
	Guard         AuthorityGuard
	// Authorize checks exact current remote scope, lease/floors, generation,
	// epoch, delegation hash, certificates and deny policy at the supplied
	// common physical interval. It is distinct from immutable proof checking.
	Authorize func(context.Context, serviceorigin.VerifiedRecord, trustedtime.Interval) (AuthorityWindow, error)
	// AdmitSchema must explicitly allow the original schema and any tombstone.
	// It may accept an expired sample as high-water, never expired authority.
	AdmitSchema   func(context.Context, serviceorigin.VerifiedRecord) error
	MaxProofBytes int64
	MaxEntries    int
}

// RemoteJournal is purpose-separated storage. It exposes no epoch reservation
// or local publisher admission method. The protected local Journal API and its
// version-one disk format remain unchanged.
type RemoteJournal struct {
	base    *Journal
	cfg     RemoteConfig
	faulted atomic.Bool
}

type RecordDescriptor struct {
	Reference       proofref.Reference `json:"reference"`
	ExpiresAtUnixNS int64              `json:"expires_at_unix_ns"`
	Tombstone       bool               `json:"tombstone"`
}

func (d RecordDescriptor) OrderKey() string { return digest([]byte(componentKey(d.Reference.Key))) }

type RecordPage struct {
	Entries []RecordDescriptor
	More    bool
}
type RecordExport struct {
	Descriptor RecordDescriptor
	Envelope   []byte
}

// ExportAccess authorizes transfer of historical register state, not current
// sample visibility. The host checks the current peer and exact origin-source
// exchange grants, including empty pages. Expired samples and tombstones may
// be transferred, but their proofs still need current authority on new import.
type ExportAccess struct {
	Guard     AuthorityGuard
	Authorize func(context.Context, []proofref.Source, trustedtime.Interval) (AuthorityWindow, error)
}
