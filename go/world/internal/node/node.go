// Package node serves authenticated, durable World replicas over bounded HTTP.
// It is currently restricted to simulation deployments.
package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/taskrelay"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const AuthorizationHeader = "X-World-Authorization"
const MaxBodyBytes = 128 << 10
const MaxResponseBytes = 16 << 20

type Config struct {
	TaskHistory               *taskhistory.Store
	ExportTaskHistory         bool
	Trace                     *diagnostics.Collector
	MissionClock              *MissionClockConfig
	AnnotationClock           *AnnotationClockConfig
	Metrics                   *runtimeobs.Collector
	Audience                  string
	TaskRelay                 *taskrelay.Store
	TaskRoutes                map[string]taskrelay.Route
	TaskStatusCredentials     Credentials
	SupervisionEnabled        bool
	Evidence                  *evidence.Store
	Tasks                     *tasking.Store
	Proposals                 *proposals.Store
	ProposalInbox             *proposalreplica.Store
	ProposalExportCredentials *Credentials
	TaskAssignee              string
	TaskFrames                []world.FrameRef
	Tenant                    string
	Site                      string
	ProofPath                 string
	RevocationPath            string
	MaxRecords                int
}

type Node struct {
	replicaStatusMu     sync.Mutex
	replicaPeers        map[string]ReplicaPeerStatus
	missionClockMu      sync.Mutex
	missionClockLast    time.Time
	missionClockBoot    string
	annotationClockMu   sync.Mutex
	annotationClockLast time.Time
	annotationClockBoot string
	relayMu             sync.Mutex
	cfg                 Config
	store               *store.Store
	verifier            *authority.Verifier
	mu                  sync.Mutex
	proofs              map[string]authority.SignedRecord
	client              *http.Client
	repairKey           [32]byte
	repairIn            sync.Mutex
	repairOut           sync.Mutex
	controlActive       atomic.Int64
	controlMu           sync.Mutex
	controlIdle         chan struct{}
	repairProgress      map[string]*repairProgress
	selectedWatchers    atomic.Int64
}

type QueryResponse struct {
	ReplicaScope *selection.ReplicaScope `json:"replica_scope,omitempty"`
	Components   []world.Component       `json:"components"`
	Cursor       string                  `json:"cursor"`
}
type ReplicationResponse struct {
	ReplicaScope *selection.ReplicaScope  `json:"replica_scope,omitempty"`
	Records      []authority.SignedRecord `json:"records"`
}
type Peer struct {
	URL      string `json:"url"`
	Audience string `json:"audience"`
}
type Credentials struct {
	Grant      authority.SignedGrant
	PrivateKey ed25519.PrivateKey
}

func newPeerClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func New(cfg Config, s *store.Store, verifier *authority.Verifier) (*Node, error) {
	if cfg.Tenant == "" || cfg.Site == "" || cfg.ProofPath == "" || cfg.MaxRecords <= 0 || s == nil || verifier == nil {
		return nil, errors.New("invalid node configuration")
	}
	if cfg.Evidence != nil && !cfg.Evidence.MatchesScope(cfg.Tenant, cfg.Site) {
		return nil, errors.New("evidence store scope mismatch")
	}
	if cfg.RevocationPath == "" {
		cfg.RevocationPath = cfg.ProofPath + ".revocations"
	}
	if cfg.Tasks != nil && (cfg.TaskAssignee == "" || len(cfg.TaskFrames) == 0) {
		return nil, errors.New("task assignee and frames required")
	}
	if cfg.SupervisionEnabled {
		g := cfg.TaskStatusCredentials.Grant.Grant
		if cfg.Tasks == nil || cfg.Audience == "" || g.TaskStatusAudience != cfg.Audience || g.TaskStatusAssignee != cfg.TaskAssignee || len(cfg.TaskStatusCredentials.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(cfg.TaskStatusCredentials.PrivateKey[32:], g.PublicKey) {
			return nil, errors.New("supervision signer must match the local assignee")
		}
		if _, err := verifier.VerifyGrant(cfg.TaskStatusCredentials.Grant, "supervision-server"); err != nil {
			return nil, fmt.Errorf("supervision server authority: %w", err)
		}
	}
	if cfg.TaskRelay != nil {
		if cfg.Audience == "" || len(cfg.TaskRoutes) == 0 {
			return nil, errors.New("task relay requires local audience and configured routes")
		}
		if cfg.Tasks != nil {
			route, ok := cfg.TaskRoutes[cfg.Audience]
			g := cfg.TaskStatusCredentials.Grant.Grant
			if !ok || route.Assignee != cfg.TaskAssignee || route.StatusPrincipal != g.Principal || g.TaskStatusAudience != cfg.Audience || g.TaskStatusAssignee != cfg.TaskAssignee || len(cfg.TaskStatusCredentials.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(cfg.TaskStatusCredentials.PrivateKey[32:], g.PublicKey) {
				return nil, errors.New("local task relay signer does not match configured assignee route")
			}
			if _, err := verifier.VerifyGrant(cfg.TaskStatusCredentials.Grant, "task-status"); err != nil {
				return nil, fmt.Errorf("task status authority: %w", err)
			}
		}
	}
	n := &Node{cfg: cfg, store: s, verifier: verifier, proofs: map[string]authority.SignedRecord{}, client: newPeerClient()}
	if cfg.Tasks != nil && cfg.Tasks.ControlContext() != nil {
		if err := verifier.VerifyMissionControlContext(*cfg.Tasks.ControlContext()); err != nil {
			return nil, err
		}
	}
	if err := n.configureTaskHistory(); err != nil {
		return nil, err
	}
	if err := n.configureProposalInbox(); err != nil {
		return nil, err
	}
	if err := n.configureProposals(); err != nil {
		return nil, err
	}
	if _, err := rand.Read(n.repairKey[:]); err != nil {
		return nil, err
	}
	if err := n.configureMissionClock(); err != nil {
		return nil, err
	}
	if err := n.configureAnnotationClock(); err != nil {
		return nil, err
	}
	data, err := readBoundedFile(cfg.ProofPath, MaxResponseBytes)
	if err == nil {
		var saved struct {
			Version int                      `json:"version"`
			Records []authority.SignedRecord `json:"records"`
		}
		if err = decode(data, &saved); err != nil {
			return nil, fmt.Errorf("proof state: %w", err)
		}
		limit := cfg.MaxRecords + 1
		if saved.Version == 2 {
			limit = cfg.MaxRecords + store.MaxRepairRecords
		}
		if (saved.Version != 1 && saved.Version != 2) || len(saved.Records) > limit {
			return nil, errors.New("invalid proof state version or size")
		}
		for _, r := range saved.Records {
			if !s.AcceptsKey(r.Component.Key) {
				return nil, subscription.ErrExcluded
			}
			d, e := recordDigest(r.Component)
			if e != nil {
				return nil, e
			}
			if _, exists := n.proofs[d]; exists {
				return nil, errors.New("duplicate proof")
			}
			n.proofs[d] = r
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	records, err := s.Records()
	if err != nil {
		return nil, err
	}
	if len(records) > cfg.MaxRecords {
		return nil, errors.New("replica exceeds node record limit")
	}
	annotationIDs := map[world.Key]bool{}
	for _, c := range records {
		d, e := recordDigest(c)
		if e != nil {
			return nil, e
		}
		if _, ok := n.proofs[d]; !ok {
			return nil, errors.New("replica record has no durable origin proof")
		}
		if annotations.Reserved(c) {
			key := annotationIdentity(c.Key)
			if annotationIDs[key] {
				return nil, ErrAnnotationConflict
			}
			annotationIDs[key] = true
			proof := n.proofs[d]
			proof.Component = c
			if err := verifier.VerifyAnnotationHistory(proof); err != nil {
				return nil, fmt.Errorf("stored annotation: %w", err)
			}
		}
	}
	if err = n.pruneRegionalProofs(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": "simulation"})
	})
	mux.HandleFunc("/v1/components", n.components)
	mux.HandleFunc("/v1/sites", n.sites)
	mux.HandleFunc("/v1/zones", n.zones)
	mux.HandleFunc("/v1/annotations/clock", n.annotationClock)
	mux.HandleFunc("/v1/tasks/clock", n.missionClock)
	mux.HandleFunc("/v1/annotations", n.annotations)
	mux.HandleFunc("/v1/annotations/views", n.annotationViews)
	mux.HandleFunc("/v1/annotations/epoch", n.annotationEpoch)
	mux.HandleFunc("/v1/watch", n.watch)
	mux.HandleFunc("/v1/query", n.selectedQuery)
	mux.HandleFunc("/v1/replica/status", n.replicaStatus)
	mux.HandleFunc("/v1/watch/selected", n.selectedWatch)
	mux.HandleFunc("/v1/publishers/epoch", n.epoch)
	mux.HandleFunc("/v1/replica", n.replica)
	mux.HandleFunc("/v1/repair/inventory", n.repairInventory)
	mux.HandleFunc("/v1/repair/fetch", n.repairFetch)
	mux.HandleFunc("/v1/revocations", n.revocations)
	mux.HandleFunc("/v1/tasks", n.tasks)
	mux.HandleFunc("/v1/tasks/catalog", n.taskCatalog)
	mux.HandleFunc("/v1/tasks/status", n.taskStatus)
	mux.HandleFunc("/v1/proposal-inbox", n.proposalInboxRead)
	mux.HandleFunc("/v1/proposal-inbox/replica", n.proposalReplica)
	mux.HandleFunc("/v1/proposals", n.proposals)
	mux.HandleFunc("/v1/proposals/", n.proposalGet)
	mux.HandleFunc("/v1/tasks/stop", n.taskStop)
	mux.HandleFunc("/v1/tasks/evaluate", n.taskEvaluate)
	mux.HandleFunc("/v1/tasks/report", n.taskReport)
	mux.HandleFunc("/v1/tasks/executor/start", n.taskExecutorStart)
	mux.HandleFunc("/v1/tasks/supervision/challenge", n.supervisionChallenge)
	mux.HandleFunc("/v1/tasks/supervision/ack", n.supervisionAck)
	mux.HandleFunc("/v1/task-history", n.taskHistoryRead)
	mux.HandleFunc("/v1/task-history/replica", n.taskHistoryReplica)
	mux.HandleFunc("/v1/task-relay", n.taskRelay)
	mux.HandleFunc("/v1/task-relay/replica", n.taskRelayReplica)
	mux.HandleFunc("/v1/evidence/put", n.evidencePut)
	mux.HandleFunc("/v1/evidence/get", n.evidenceGet)
	mux.HandleFunc("/v1/evidence/usage", n.evidenceUsage)
	mux.HandleFunc("/v1/evidence/pin", n.evidencePin)
	mux.HandleFunc("/v1/evidence/unpin", n.evidenceUnpin)
	mux.HandleFunc("/v1/packages/publish", n.packagePublish)
	return n.traced(n.controlFirst(n.measured(mux)))
}
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method != want {
		w.Header().Set("Allow", want)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}
func (n *Node) authorize(w http.ResponseWriter, r *http.Request, operation string, body []byte) (authority.Identity, bool) {
	header := r.Header.Get(AuthorizationHeader)
	if len(header) > 32<<10 {
		http.Error(w, "authorization too large", http.StatusUnauthorized)
		return authority.Identity{}, false
	}
	data, err := base64.StdEncoding.Strict().DecodeString(header)
	var signed authority.SignedRequest
	if err == nil {
		err = authority.DecodeStrict(data, &signed)
	}
	var identity authority.Identity
	if err == nil {
		phase := diagnostics.Begin(r.Context(), diagnostics.OuterAuthorization)
		identity, err = n.verifier.VerifyContext(r.Context(), signed, operation, body)
		phase.End(err)
	}
	if err != nil {
		http.Error(w, "authorization rejected", http.StatusUnauthorized)
		return authority.Identity{}, false
	}
	return identity, true
}
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid or oversized body", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return data, true
}
func (n *Node) components(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := n.authorize(w, r, "read", nil); !ok {
			return
		}
		response, err := n.queryContext(r.Context())
		if err != nil {
			http.Error(w, "replica unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	identity, ok := n.authorize(w, r, "publish", body)
	if !ok {
		return
	}
	var record authority.SignedRecord
	if err := authority.DecodeStrict(body, &record); err != nil {
		http.Error(w, "invalid record", http.StatusBadRequest)
		return
	}
	if identity.Source != record.Component.Key.Source {
		http.Error(w, "publisher source mismatch", http.StatusForbidden)
		return
	}
	result, err := n.applyContext(r.Context(), record, true)
	if err != nil {
		http.Error(w, "record rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Result    world.Result    `json:"result"`
		Component world.Component `json:"component"`
	}{result, record.Component})
}
func (n *Node) epoch(w http.ResponseWriter, r *http.Request) {
	n.epochFor(w, r, "publish")
}
func (n *Node) epochFor(w http.ResponseWriter, r *http.Request, operation string) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, operation, body)
	if !ok {
		return
	}
	var empty struct{}
	if err := decode(body, &empty); err != nil {
		http.Error(w, "expected empty JSON object", http.StatusBadRequest)
		return
	}
	n.lock()
	epoch, err := n.store.NextEpoch(id.Source)
	n.mu.Unlock()
	if err != nil {
		http.Error(w, "epoch allocation failed", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"epoch": epoch})
}
func (n *Node) query() (QueryResponse, error) {
	return n.queryContext(context.Background())
}
func (n *Node) queryContext(ctx context.Context) (QueryResponse, error) {
	n.lock()
	defer n.mu.Unlock()
	records, err := n.store.Snapshot()
	if err != nil {
		return QueryResponse{}, err
	}
	amended, err := n.amendedOriginalsLocked()
	if err != nil {
		return QueryResponse{}, err
	}
	visible := make([]world.Component, 0, len(records))
	for _, c := range records {
		if amended[c.Key] && !annotations.IsAmendment(c) {
			continue
		}
		d, err := recordDigest(c)
		if err != nil {
			return QueryResponse{}, err
		}
		proof, ok := n.proofs[d]
		if !ok {
			return QueryResponse{}, errors.New("missing origin proof")
		}
		if _, err = n.verifyRecordContext(ctx, proof); err == nil {
			visible = append(visible, c)
		}
	}
	data, err := json.Marshal(visible)
	if err != nil {
		return QueryResponse{}, err
	}
	hash := sha256.Sum256(data)
	response := QueryResponse{ReplicaScope: n.store.Subscription().Scope(), Components: visible, Cursor: hex.EncodeToString(hash[:])}
	if _, err := marshalResponse(response); err != nil {
		return QueryResponse{}, err
	}
	return response, nil
}
func (n *Node) watch(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	identity, ok := n.authorize(w, r, "read", nil)
	if !ok {
		return
	}
	data, _ := base64.StdEncoding.Strict().DecodeString(r.Header.Get(AuthorizationHeader))
	var request authority.SignedRequest
	if err := authority.DecodeStrict(data, &request); err != nil {
		http.Error(w, "invalid authority", http.StatusUnauthorized)
		return
	}
	wait := int64(1000)
	if raw := r.URL.Query().Get("timeout_ms"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 || v > 5000 {
			http.Error(w, "timeout_ms must be 0..5000", http.StatusBadRequest)
			return
		}
		wait = v
	}
	previous := r.URL.Query().Get("cursor")
	if len(previous) > 64 {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	if identity.NotAfter.Before(deadline) {
		deadline = identity.NotAfter
	}
	requestEnd := time.UnixMilli(request.ExpiresAtUnixMS)
	if requestEnd.Before(deadline) {
		deadline = requestEnd
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timedOut := false
	for {
		if _, err := n.verifier.VerifyGrant(request.Grant, "read"); err != nil || !time.Now().Before(requestEnd) {
			http.Error(w, "watch authority expired or revoked", http.StatusUnauthorized)
			return
		}
		response, err := n.queryContext(r.Context())
		if err != nil {
			http.Error(w, "replica unavailable", http.StatusServiceUnavailable)
			return
		}
		if response.Cursor != previous || timedOut {
			writeJSON(w, http.StatusOK, response)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			timedOut = true
		case <-ticker.C:
		}
	}
}

// LoadRevocations selects the latest full snapshot before a verifier checks it
// against its durable high-water mark. Malformed persisted data fails closed.
func LoadRevocations(path string, initial authority.SignedRevocations) (authority.SignedRevocations, error) {
	data, err := readBoundedFile(path, authority.MaxEnvelopeBytes)
	if errors.Is(err, os.ErrNotExist) {
		return initial, nil
	}
	if err != nil {
		return authority.SignedRevocations{}, err
	}
	var saved authority.SignedRevocations
	if err = authority.DecodeStrict(data, &saved); err != nil {
		return authority.SignedRevocations{}, err
	}
	if saved.Revocations.Revision >= initial.Revocations.Revision {
		return saved, nil
	}
	return initial, nil
}
func (n *Node) revocations(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var update authority.SignedRevocations
	if err := authority.DecodeStrict(body, &update); err != nil {
		http.Error(w, "invalid revocation update", http.StatusBadRequest)
		return
	}
	n.lock()
	defer n.mu.Unlock()
	if err := n.verifier.ValidateRevocations(update); err != nil {
		http.Error(w, "revocation update rejected", http.StatusUnauthorized)
		return
	}
	if err := writeAtomic(n.cfg.RevocationPath, body); err != nil {
		http.Error(w, "revocation persistence failed", http.StatusServiceUnavailable)
		return
	}
	if err := n.verifier.UpdateRevocations(update); err != nil {
		http.Error(w, "revocation update failed", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
}
func (n *Node) replica(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if !n.admitRepair(w, r) {
		return
	}
	defer n.repairIn.Unlock()
	if _, ok := n.authorize(w, r, "replicate", nil); !ok {
		return
	}
	records, err := n.exportContext(r.Context())
	if err != nil {
		http.Error(w, "replica export failed", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, ReplicationResponse{ReplicaScope: n.store.Subscription().Scope(), Records: records})
}
func (n *Node) export() ([]authority.SignedRecord, error) {
	return n.exportContext(context.Background())
}
func (n *Node) exportContext(ctx context.Context) ([]authority.SignedRecord, error) {
	n.lock()
	records, err := n.store.Records()
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	captured := make([]authority.SignedRecord, 0, len(records))
	for _, c := range records {
		d, err := recordDigest(c)
		if err != nil {
			n.mu.Unlock()
			return nil, err
		}
		proof, ok := n.proofs[d]
		if !ok {
			n.mu.Unlock()
			return nil, errors.New("missing origin proof")
		}
		// Relay age can increase, but never changes the origin signature.
		proof.Component = c
		captured = append(captured, proof)
	}
	data, err := json.Marshal(captured)
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		return nil, errors.New("replica response exceeds budget")
	}
	if err = authority.DecodeStrictLimit(data, &captured, MaxResponseBytes); err != nil {
		return nil, err
	}
	// Legacy full export retains its response shape, but releases the node lock
	// before cryptographic work and yields to control between bounded groups.
	out := make([]authority.SignedRecord, 0, len(captured))
	for offset := 0; offset < len(captured); offset += store.MaxRepairRecords {
		if n.controlActive.Load() != 0 {
			return nil, errRepairBusy
		}
		end := offset + store.MaxRepairRecords
		if end > len(captured) {
			end = len(captured)
		}
		group := captured[offset:end]
		if _, err := n.verifyRepairRecordsContext(ctx, group); err == nil {
			out = append(out, group...)
		} else if errors.Is(err, authority.ErrDenied) {
			for _, record := range group {
				if _, err := n.verifyRepairRecordsContext(ctx, []authority.SignedRecord{record}); err == nil {
					out = append(out, record)
				} else if !errors.Is(err, authority.ErrDenied) {
					return nil, err
				}
			}
		} else {
			return nil, err
		}
	}
	if _, err := marshalResponse(ReplicationResponse{ReplicaScope: n.store.Subscription().Scope(), Records: out}); err != nil {
		return nil, err
	}
	return out, nil
}
func (n *Node) apply(record authority.SignedRecord, local bool) (world.Result, error) {
	return n.applyContext(context.Background(), record, local)
}
func (n *Node) applyContext(ctx context.Context, record authority.SignedRecord, local bool) (world.Result, error) {
	if record.Component.Key.Tenant != n.cfg.Tenant || record.Component.Key.Site != n.cfg.Site {
		return "", world.ErrWrongScope
	}
	if !n.store.AcceptsKey(record.Component.Key) {
		return "", subscription.ErrExcluded
	}
	identity, err := n.verifyRecordContext(ctx, record)
	if err != nil {
		return "", err
	}
	if identity.Source != record.Component.Key.Source {
		return "", world.ErrWrongSource
	}
	n.lock()
	defer n.mu.Unlock()
	// A node revocation can be installed while the first proof pass waits for
	// this lock. Check current amendment authority inside that same fence.
	if annotations.IsAmendment(record.Component) || n.store.Subscription().Regional() {
		identity, err = n.verifyRecordContext(ctx, record)
		if err != nil {
			return "", err
		}
		identity, err = n.regionalCurrent(ctx, record, identity)
		if err != nil {
			return "", err
		}
	}
	if local {
		if annotations.Reserved(record.Component) {
			// Worldd frame activation is static. Future node activation APIs
			// must take n.mu too, so this admission check stays serialized.
			matches, err := n.store.MatchesFrame(record.Component.Frame)
			if err != nil {
				return "", err
			}
			if !matches {
				return "", ErrAnnotationFrame
			}
		}
	}
	// This callback runs under both the node and store locks. It must not
	// reenter either or perform authority verification. The store stages an
	// observation checkpoint before preparing proofs, then commits the
	// accepted world update once. Known denials retain that observed floor.
	result, err := n.store.ApplyPrepared(identity.Source, record.Component, func(state store.PublicationState) error {
		if (annotations.IsAmendment(record.Component) || n.store.Subscription().Regional()) && !time.Now().Before(identity.NotAfter) {
			return authority.ErrDenied
		}
		if local && (state.Epoch == 0 || record.Component.Version.Epoch != state.Epoch) {
			return ErrPublisherEpoch
		}
		if annotations.Reserved(record.Component) {
			candidate, err := recordDigest(record.Component)
			if err != nil {
				return err
			}
			for _, c := range state.Records {
				if annotationIdentity(c.Key) == annotationIdentity(record.Component.Key) {
					prior, err := recordDigest(c)
					if err != nil {
						return err
					}
					if prior != candidate {
						return ErrAnnotationConflict
					}
				}
			}
		}
		// Store is authoritative. Keep current proofs plus candidate so a crash
		// at either side of the world commit can recover without losing a proof.
		// Duplicates can refresh signed grants or shorten unsigned age, so they
		// must still prepare proofs and pass through the ordinary world merge.
		proofs := map[string]authority.SignedRecord{}
		for _, c := range state.Records {
			d, err := recordDigest(c)
			if err != nil {
				return err
			}
			p, ok := n.proofs[d]
			if !ok {
				return errors.New("missing current proof")
			}
			proofs[d] = p
		}
		d, err := recordDigest(record.Component)
		if err != nil {
			return err
		}
		if n.store.Retains(record.Component) {
			proofs[d] = record
		}
		if len(proofs) > n.cfg.MaxRecords+1 {
			return errors.New("origin proof capacity reached")
		}
		if err = n.saveProofs(proofs); err != nil {
			return err
		}
		n.proofs = proofs
		if (annotations.IsAmendment(record.Component) || n.store.Subscription().Regional()) && !time.Now().Before(identity.NotAfter) {
			return authority.ErrDenied
		}
		return nil
	})
	if err == nil {
		err = n.pruneRegionalProofs()
	}
	return result, err
}

func recordDigest(c world.Component) (string, error) {
	c.Timing.AgeHint = 0
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func (n *Node) saveProofs(proofs map[string]authority.SignedRecord) (resultErr error) {
	var started time.Time
	if n.cfg.Metrics != nil {
		started = time.Now()
	}
	keys := make([]string, 0, len(proofs))
	for k := range proofs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	records := make([]authority.SignedRecord, 0, len(keys))
	for _, k := range keys {
		records = append(records, proofs[k])
	}
	data, err := json.Marshal(struct {
		Version int                      `json:"version"`
		Records []authority.SignedRecord `json:"records"`
	}{2, records})
	if err != nil {
		return err
	}
	if len(data) > MaxResponseBytes {
		return errors.New("origin proof bytes exhausted")
	}
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.Add(runtimeobs.ProofStoreSerializedBytes, uint64(len(data)))
		defer func() {
			n.cfg.Metrics.Observe(runtimeobs.ProofStoreCommit, time.Since(started))
			if resultErr != nil {
				n.cfg.Metrics.Increment(runtimeobs.ProofStoreCommitFailures)
			}
		}()
	}
	return writeAtomic(n.cfg.ProofPath, data)
}
func readBoundedFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".proof-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func decode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

// All legacy response budgets include their complete wrapper, scope and newline.
func marshalResponse(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data)+1 > MaxResponseBytes {
		return nil, errors.New("response exceeds budget")
	}
	return data, nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := marshalResponse(value)
	if err != nil {
		http.Error(w, "response exceeds budget", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// Sync performs one bounded pull from a peer. Every relayed record is verified
// against its origin grant/signature before admission; peer identity is not origin.
func (n *Node) Sync(ctx context.Context, peer Peer, credentials Credentials) error {
	if n.cfg.TaskRelay == nil && n.cfg.ProposalInbox == nil && n.cfg.TaskHistory == nil {
		return n.syncWorld(ctx, peer, credentials)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	calls := []func() error{func() error { return n.syncWorld(bounded, peer, credentials) }}
	if n.cfg.TaskRelay != nil {
		calls = append(calls, func() error { return n.syncTaskRelay(bounded, peer, credentials) })
	}
	if n.cfg.TaskHistory != nil {
		calls = append(calls, func() error { return n.syncTaskHistory(bounded, peer, credentials) })
	}
	if n.cfg.ProposalInbox != nil {
		calls = append(calls, func() error { return n.SyncProposalInbox(bounded, peer, credentials) })
	}
	result := make(chan error, len(calls))
	for _, call := range calls {
		go func(f func() error) { result <- f() }(call)
	}
	var failures []error
	for range calls {
		failures = append(failures, <-result)
	}
	return errors.Join(failures...)
}

func (n *Node) syncWorld(ctx context.Context, peer Peer, credentials Credentials) (resultErr error) {
	if n.cfg.Metrics != nil {
		n.cfg.Metrics.Increment(runtimeobs.RepairRounds)
		defer func() {
			if resultErr != nil {
				n.cfg.Metrics.Increment(runtimeobs.RepairRoundFailures)
			}
		}()
	}
	return n.repairRound(ctx, peer, credentials)
}
func AuthorizeRequest(req *http.Request, credentials Credentials, audience, operation string, body []byte) error {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	signed, err := authority.SignRequest(credentials.Grant, credentials.PrivateKey, audience, operation, body, hex.EncodeToString(nonce), time.Now(), 30*time.Second)
	if err != nil {
		return err
	}
	data, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	req.Header.Set(AuthorizationHeader, base64.StdEncoding.EncodeToString(data))
	return nil
}
