// Package replication implements bounded native-agent state repair. Its host
// supplies authenticated transport, current commissioned policy and durable
// origin verification/admission; no transport hint is publication authority.
package replication

import (
	"context"
	"errors"
	"io"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
)

const (
	Version             = 1
	MaxInventoryEntries = 64
	MaxFetchEntries     = 8
	MaxInventoryBytes   = 128 << 10
	MaxPageBytes        = 512 << 10
	MaxCursorBytes      = 2048
	MaxEnvelopeBytes    = 384 << 10
	MaxFinalChecks      = 4
	MaxLatestItems      = MaxFetchEntries
	MaxLatestBytes      = MaxPageBytes
	ScalarComponent     = "wendy.sensor.scalar.v1"
)

var (
	ErrDenied    = errors.New("replica access denied")
	ErrProtocol  = errors.New("invalid replica protocol")
	ErrBusy      = errors.New("replica peer operation already active")
	ErrCapacity  = errors.New("replica quota exceeded")
	ErrUncertain = errors.New("replica admission outcome uncertain")
	// ErrRecordWindowDeferred is supplied only by a host store that proved an
	// original record-window refusal before that record could commit. It says
	// nothing about other metadata writes. Never use it for uncertain commits,
	// failed postcommit checks, clock faults or arbitrary admission errors.
	ErrRecordWindowDeferred = errors.New("replica record window deferred before commit")
)

type Endpoint struct{ Tenant, Site, Device, ExecutionDomain, SampleClockDomain string }

// BindingSHA256 identifies a stable authenticated credential incarnation,
// typically the peer certificate fingerprint. It must survive fresh TLS
// connections to that same identity, and change on identity replacement.
// A per-session nonce would force continuation to restart at every dial.
// The host cancels outgoing connections when its identity is invalidated.
type Identity struct{ Device, BindingSHA256 string }
type Direction uint8

const (
	Import Direction = iota + 1
	Export
)

// Generation is a nonzero host policy incarnation, not a publication epoch.
// Sources must all refer to the connected origin on import and this node on
// export. They do not authorize a peer to forward a third origin's records.
type Access struct {
	Generation uint64
	Sources    []proofref.Source
}
type Actor struct{ Tenant, Site, Device, App, Service string }
type Kind uint8

const (
	Floor Kind = iota + 1
	Lease
	Record
)

type AuthorityHint struct {
	Actor                            Actor
	Revision, Epoch, ThroughRevision uint64
	EnvelopeSHA256                   string
}

// Entry is a comparable transfer hint. Exactly one of Authority/Reference is
// populated. ExpiresAtUnixNS is in the original sample-clock domain and can
// only shorten a deadline recomputed from the authenticated original proof.
type Entry struct {
	Kind            Kind
	Authority       AuthorityHint
	Reference       proofref.Reference
	ExpiresAtUnixNS int64
	Tombstone       bool
}
type Item struct {
	Entry    Entry
	Envelope []byte
}
type InventoryPage struct {
	Entries []Entry
	More    bool
}

// Store callbacks do only local work. They must never acquire network state.
// Inventory returns sorted entries strictly after the opaque OrderKey and no
// more than limit. Floor and lease entries exist independently of records.
// Fetch must return the exact requested entry or found=false if it changed.
// Need never accepts an inventory hint as authenticated state.
// Admit verifies original cryptography, schema, source authority and deadlines,
// persists atomically, and runs finalCheck at its final commit boundary and
// after commit. It may invoke the check synchronously up to MaxFinalChecks
// times; it must propagate failures and never retain the callback. The
// caller must not acknowledge ambiguous commits; failures stop this round.
// Explicit ErrRecordWindowDeferred or AdmissionNotStarted outcomes may stop the
// round without an unknown outcome; the existing scheduler may later revisit
// the unchanged original. AdmissionNotStarted requires zero final checks.
// Remote input must never reserve or advance a local publisher epoch.
type Store interface {
	Inventory(context.Context, []proofref.Source, string, int) (InventoryPage, error)
	Fetch(context.Context, Entry) (Item, bool, error)
	Need(context.Context, Entry) (bool, error)
	Admit(context.Context, Item, func(context.Context) error) error
}
type Connection struct {
	Identity Identity
	Client   replicav1.WorldReplicaServiceClient
	Close    io.Closer
}
type Limits struct {
	MaxPeers, MaxPages, MaxEntries, MaxBytes int
	RoundTimeout, RequestTimeout             time.Duration
}
type Config struct {
	Endpoint     Endpoint
	Store        Store
	IncomingPeer func(context.Context) (Identity, error)
	Authorize    func(context.Context, Identity, Direction) (Access, error)
	Current      func(context.Context, Identity, Direction, uint64) error
	// RemoteAuthority is mandatory even when the transport certificate is valid.
	// It runs before admission and inside the store's final check. It must not
	// mutate Item, enter Store again, or make a network call.
	RemoteAuthority func(context.Context, Item) error
	// AdmissionFailure is an optional local diagnostic invoked after a failed
	// Admit returns. It receives no envelope and must not change authority or
	// retry the operation. FinalChecks counts checks invoked by the store.
	AdmissionFailure func(kind Kind, finalChecks int, err error)
	// ReadEvidence is optional and performs only a local, source-owned range
	// read under the host's current peer/export guard. It must return owned
	// bytes and the original retention deadline, never fetch another peer.
	ReadEvidence func(context.Context, Identity, uint64, proofref.Source, serviceevidence.GetRequest) (serviceevidence.GetResponse, error)
	Dial         func(context.Context, string) (Connection, error)
	Limits       Limits
}
type Result struct {
	Pages, Entries, Fetched, Admitted, Unavailable, Bytes int
	Skipped                                               int
	Complete                                              bool
	// Deferred includes unavailable items, explicit precommit record-window
	// refusals and host admission guards that were never entered. None changes
	// original timestamps or lifetime bounds.
	Deferred bool
	// An unclassified Admit failure may already have persisted. Only the
	// explicit precommit record-window or unentered-guard refusal is exempt; unknown mutations
	// are never retried or reported as successful repair completion. SendLatest
	// also sets this on an unobserved remote delivery (lost/malformed reply).
	// That sender-side case does not imply a fault in the local source/store;
	// retire that batch while allowing new values and independent repair.
	UnknownOutcome bool
}
