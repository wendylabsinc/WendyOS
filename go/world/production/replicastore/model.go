// Package replicastore joins native repair to protected authority and journals.
// An instance is commissioned to one exact peer; it is not an origin router.
package replicastore

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var (
	ErrDenied  = errors.New("replica store access denied")
	ErrFaulted = errors.New("replica store outcome uncertain; reopen required")
	ErrClosed  = errors.New("replica store closed")
)

type Config struct {
	Endpoint       replication.Endpoint
	Peer           replication.Identity
	Generation     uint64
	Import, Export []proofref.Source
	Local          *servicejournal.Journal
	Authority      *sourceauthority.Store
	RemotePath     string
	Replica        world.Config
	Clock          trustedtime.Provider
	Certificates   *wendycert.DeviceVerifier
	MaxEntries     int
	MaxProofBytes  int64
	// Guard holds host identity then the common remote-operation fence. Every
	// identity, policy, authority floor/lease and lifecycle mutation uses these
	// same fences. It runs its callback once synchronously and propagates errors.
	Guard servicejournal.AuthorityGuard
	// Authorize runs inside Guard. It must verify this exact stable peer binding
	// and policy generation, exact direction/selectors and current deny policy.
	// No callback may acquire an earlier lock, enter a store, or use the network.
	Authorize func(context.Context, replication.Identity, uint64, replication.Direction, []proofref.Source, trustedtime.Interval) (servicejournal.AuthorityWindow, error)
	// AdmitSchema adds explicit commissioning rules to the built-in scalar,
	// clock-domain and frame checks. A tombstone is also passed to this callback.
	AdmitSchema func(context.Context, serviceorigin.VerifiedRecord) error
}
type Store struct {
	cfg             Config
	remote          *servicejournal.RemoteJournal
	closed, faulted atomic.Bool
}

var _ replication.Store = (*Store)(nil)

// Create initializes only the separate remote journal. Local and authority
// stores are explicitly initialized/owned by the host and are never recreated.
func Create(cfg Config) (*Store, error) { return open(cfg, false) }
func Open(cfg Config) (*Store, error)   { return open(cfg, true) }
func (s *Store) Close() error {
	if s == nil || s.closed.Swap(true) {
		return nil
	}
	return s.remote.Close()
}
