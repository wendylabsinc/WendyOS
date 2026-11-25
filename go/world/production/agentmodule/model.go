// Package agentmodule connects commissioned publication policy, agent-held
// device signing, per-service sockets and a durable local World journal.
package agentmodule

import (
	"context"
	"crypto/mldsa"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

var ErrDenied = errors.New("commissioned World publication unavailable")

type Grant struct {
	App       string                       `json:"app"`
	Service   string                       `json:"service"`
	Publish   []serviceorigin.PublishScope `json:"publish"`
	Subscribe []serviceaccess.Source       `json:"subscribe,omitempty"`
}

// Policy is installed by a trusted host administrator, separately from app
// deployment. It is not an operator role grant or permission to move hardware.
// Replacing it invalidates the module; activation requires a fresh process.
type Policy struct {
	Version            int              `json:"version"`
	Tenant             string           `json:"tenant"`
	Site               string           `json:"site"`
	Device             string           `json:"device"`
	ExecutionDomain    string           `json:"execution_domain"`
	SampleClockDomain  string           `json:"sample_clock_domain"`
	Frames             []world.FrameRef `json:"frames"`
	Grants             []Grant          `json:"grants"`
	NotBefore          time.Time        `json:"not_before"`
	NotAfter           time.Time        `json:"not_after"`
	DeniedCertificates []string         `json:"denied_certificates,omitempty"`
	DeniedKeys         []string         `json:"denied_keys,omitempty"`
}

type IdentitySnapshot struct {
	Token       uint64
	ChainDER    [][]byte
	Key         *mldsa.PrivateKey
	Invalidated <-chan struct{}
}

// Identity belongs to the agent's provisioning service. WithCurrent orders a
// journal commit against certificate rotation/reset. Its callback must not
// call Snapshot or another provisioning API that takes the same lock.
type Identity interface {
	Snapshot(context.Context) (IdentitySnapshot, error)
	// Current must be lock-free and callable from inside WithCurrent. The
	// provisioning adapter uses an atomic generation, never its state mutex.
	Current(uint64) bool
	WithCurrent(uint64, func() error) error
}

// ClockIdentity is an optional agent capability for clock and immutable public
// source-authority observations only. It grants no observation freshness.
// The callback retains the exact current identity until it returns, but may
// overlap an ordinary WithCurrent callback. Identity mutation and Snapshot
// remain exclusive. Callbacks must never reenter provisioning, persist data,
// change service lifecycle, or acquire the ordinary World operation fence.
// Hosts without this capability retain the original WithCurrent behavior.
type ClockIdentity interface {
	Identity
	WithCurrentClock(uint64, func() error) error
}

type Config struct {
	Policy             Policy
	Roots              [][]byte
	Directory          string
	JournalPath        string
	ProofArchivePath   string
	CreateProofArchive bool
	EvidenceDirectory  string
	CaptureSource      *CaptureSource
	NativeCameras      []NativeCameraBinding
	CreateJournal      bool
	Replication        *ReplicaConfig
	CreateReplication  bool
	UID, GID           int
	Limits             appservice.Limits
}

// CaptureSource pins the scene incarnation independently of app assertions.
// It is required for rich simulation captures, including scalar-only records.
type CaptureSource struct {
	RunID         string
	BootID        string
	PackageSHA256 string
	AnchorUnixNS  int64
	Frame         world.FrameRef
}
type Dependencies struct {
	// Trace is optional host-owned diagnostic state, never authority.
	Trace    *rpctrace.Sink
	Identity Identity
	Clock    trustedtime.Provider
	// SampleNow is distinct from certificate validity time. Production wiring
	// supplies conservative authenticated UTC or an explicit simulation clock.
	// Clock and SampleNow must support concurrent observations. An optional
	// ClockIdentity can let clock readers overlap an ordinary data operation.
	SampleNow   func() time.Time
	SampleClose func() error
	PeerCheck   appservice.PeerCheck
	// PolicyCurrent fails if the administrator's policy/roots/config changed.
	// It must be bounded, fail closed, and never call the manager lifecycle.
	PolicyCurrent func(context.Context) error
}
