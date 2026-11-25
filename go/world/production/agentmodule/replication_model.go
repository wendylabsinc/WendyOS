package agentmodule

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replicastore"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

// ReplicaConfig is immutable administrator commissioning, separate from an
// application's read entitlement and from transport enrollment. Stores must
// already exist on ordinary startup. No missing database is recreated.
type ReplicaConfig struct {
	AuthorityPath string        `json:"authority_path"`
	Peers         []ReplicaPeer `json:"peers"`
}

type ReplicaPeer struct {
	Device           string `json:"device"`
	CredentialSHA256 string `json:"credential_sha256"`
	Generation       uint64 `json:"generation"`
	RemotePath       string `json:"remote_path"`
	// EvidencePath is an optional cache of original peer-owned media. Unlike
	// authority/journal state, a missing cache may be refilled from its origin.
	EvidencePath string            `json:"evidence_path,omitempty"`
	Import       []proofref.Source `json:"import"`
	Export       []proofref.Source `json:"export"`
}

// ReplicaTransport is supplied only by the private authenticated agent host.
// Chains passed to AuthenticateReplicaPeer must come from verified TLS state,
// never request bodies. Dial must use the native routed authenticated path and
// cancel connections on identity loss; no plaintext or LAN fallback is allowed.
type ReplicaTransport struct {
	IncomingPeer func(context.Context) (replication.Identity, error)
	Dial         func(context.Context, string) (replication.Connection, error)
}

type replicaPeerState struct {
	policy   ReplicaPeer
	store    *replicastore.Store
	service  *replication.Service
	evidence *serviceevidence.Store
	latest   *replicaLatestQueue
	// Public certificate bytes only; protected by Manager.mu. A current
	// transport authentication installs them, and every authority boundary
	// verifies their original validity again using authenticated World time.
	chain [][]byte
}

type replicaState struct {
	mu         sync.Mutex
	failed     atomic.Bool
	started    bool // Manager.mu
	identity   IdentitySnapshot
	transport  ReplicaTransport
	authority  *sourceauthority.Store
	peers      map[string]*replicaPeerState
	ctx        context.Context
	cancel     context.CancelFunc
	latestWake chan struct{}
}

type replicaDispatcher struct {
	replicav1.UnimplementedWorldReplicaServiceServer
	m *Manager
}

var _ replicav1.WorldReplicaServiceServer = (*replicaDispatcher)(nil)

// ReplicaSyncInterval is the pause after a completed host repair round. Live
// captures and typed results need both native transfer legs within their
// original short lifetimes. This changes scheduling only, never those bounds.
// The host still runs one round at a time, including when a round is slow.
func (m *Manager) ReplicaSyncInterval() time.Duration {
	if m != nil && m.cfg.Replication != nil {
		for _, peer := range m.cfg.Replication.Peers {
			for _, sources := range [][]proofref.Source{peer.Import, peer.Export} {
				for _, source := range sources {
					if source.Component == sensorrecord.ComponentType || source.Component == resultrecord.ComponentType {
						return 250 * time.Millisecond
					}
				}
			}
		}
	}
	return 2 * time.Second
}

func validReplicaPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func (r *ReplicaConfig) imports(p Policy, s serviceaccess.Source) bool {
	if r == nil || s.Device == "" || s.Device == p.Device || !replication.SupportedComponent(s.ComponentType) {
		return false
	}
	want := proofref.Source{Tenant: p.Tenant, Site: p.Site, Device: s.Device, App: s.App, Service: s.Service, Component: s.ComponentType, Stream: s.Stream}
	for _, peer := range r.Peers {
		if peer.Device != s.Device {
			continue
		}
		for _, source := range peer.Import {
			if source == want {
				return true
			}
		}
	}
	return false
}

func (r *ReplicaConfig) validate(p Policy, localPaths ...string) error {
	if r == nil {
		return nil
	}
	if !validReplicaPath(r.AuthorityPath) || len(r.Peers) == 0 || len(r.Peers) > 16 {
		return ErrDenied
	}
	paths := map[string]bool{r.AuthorityPath: true}
	for _, path := range localPaths {
		if path != "" {
			if paths[path] {
				return ErrDenied
			}
			paths[path] = true
		}
	}
	devices := map[string]bool{}
	for _, peer := range r.Peers {
		tenant, err := serviceaccess.DeviceTenant(peer.Device)
		digest, decodeErr := hex.DecodeString(peer.CredentialSHA256)
		if err != nil || tenant != p.Tenant || peer.Device == p.Device || devices[peer.Device] || decodeErr != nil || len(digest) != 32 || hex.EncodeToString(digest) != peer.CredentialSHA256 || peer.Generation == 0 || !validReplicaPath(peer.RemotePath) || paths[peer.RemotePath] {
			return ErrDenied
		}
		devices[peer.Device], paths[peer.RemotePath] = true, true
		if peer.EvidencePath != "" {
			if !validReplicaPath(peer.EvidencePath) || paths[peer.EvidencePath] {
				return ErrDenied
			}
			paths[peer.EvidencePath] = true
		}
		if len(peer.Import)+len(peer.Export) == 0 {
			return ErrDenied
		}
		for direction, list := range [][]proofref.Source{peer.Import, peer.Export} {
			origin := peer.Device
			if direction == 1 {
				origin = p.Device
			}
			if len(list) > 64 {
				return ErrDenied
			}
			seen := map[proofref.Source]bool{}
			for _, source := range list {
				if source.Validate() != nil || source.Tenant != p.Tenant || source.Site != p.Site || source.Device != origin || !replication.SupportedComponent(source.Component) || seen[source] {
					return ErrDenied
				}
				seen[source] = true
				if direction == 1 && !policyPublishes(p, source) {
					return ErrDenied
				}
			}
			// Delegations describe the whole publisher. An export commission
			// must cover that actor's complete supported grant; a partial lease
			// must never silently authorize its other publication components.
			if direction == 1 {
				for _, source := range list {
					for _, grant := range p.Grants {
						if grant.App != source.App || grant.Service != source.Service {
							continue
						}
						for _, pair := range grant.Publish {
							want := proofref.Source{Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: grant.App, Service: grant.Service, Component: pair.ComponentType, Stream: pair.Stream}
							if !replication.SupportedComponent(pair.ComponentType) || !seen[want] {
								return ErrDenied
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func policyPublishes(p Policy, source proofref.Source) bool {
	for _, grant := range p.Grants {
		if grant.App == source.App && grant.Service == source.Service {
			for _, pair := range grant.Publish {
				if source.Component == pair.ComponentType && source.Stream == pair.Stream {
					return true
				}
			}
		}
	}
	return false
}
