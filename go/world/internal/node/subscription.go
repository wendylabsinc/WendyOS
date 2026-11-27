package node

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
)

type ReplicaPeerStatus = subscription.PeerStatus

func (n *Node) validateRepairSubscription(version int, p *subscription.Policy) error {
	if version == RepairVersion && p == nil {
		return nil
	}
	if version != SelectiveRepairVersion || p == nil {
		return errors.New("selective repair requires version 2 and a subscription")
	}
	if p.Regional() && n.store.Subscription() != nil {
		return errors.New("regional storage repair requires a full source")
	}
	return n.validateSourceSubscription(p)
}
func (n *Node) validateSourceSubscription(p *subscription.Policy) error {
	if p == nil {
		return nil
	}
	normalized, err := p.Normalize()
	if err != nil {
		return err
	}
	if normalized.Tenant != n.cfg.Tenant || normalized.Site != n.cfg.Site {
		return subscription.ErrMismatch
	}
	*p = normalized
	return nil
}
func (n *Node) noteReplicaAttempt(key string, peer Peer) {
	n.replicaStatusMu.Lock()
	defer n.replicaStatusMu.Unlock()
	if n.replicaPeers == nil {
		n.replicaPeers = map[string]ReplicaPeerStatus{}
	}
	if _, ok := n.replicaPeers[key]; !ok && len(n.replicaPeers) >= maxRepairPeers {
		return
	}
	prior := n.replicaPeers[key]
	prior.Audience = peer.Audience
	prior.LastAttempt = time.Now().UTC()
	prior.LastOutcome = "running"
	n.replicaPeers[key] = prior
}
func (n *Node) noteReplicaSource(key string, p *subscription.Policy) {
	n.replicaStatusMu.Lock()
	defer n.replicaStatusMu.Unlock()
	prior, ok := n.replicaPeers[key]
	if !ok {
		return
	}
	prior.SourceScopeKnown = true
	prior.SourcePartial = p != nil
	prior.SourcePolicySHA256 = p.Digest()
	n.replicaPeers[key] = prior
}
func (n *Node) noteReplicaResult(key string, result error) {
	n.replicaStatusMu.Lock()
	defer n.replicaStatusMu.Unlock()
	prior, ok := n.replicaPeers[key]
	if !ok {
		return
	}
	prior.LastOutcome = "failed"
	if result == nil {
		now := time.Now().UTC()
		prior.LastCompleted = &now
		prior.LastOutcome = "complete"
	} else if errors.Is(result, errRepairBusy) {
		prior.LastOutcome = "deferred"
	}
	n.replicaPeers[key] = prior
}
func (n *Node) replicaStatus(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	cp, err := n.store.RepairSnapshot()
	if err != nil {
		http.Error(w, "replica status unavailable", 503)
		return
	}
	policy := n.store.Subscription()
	out := subscription.Status{Version: 1, Tenant: n.cfg.Tenant, Site: n.cfg.Site, Mode: "full", Policy: policy, PolicySHA256: policy.Digest(), RetainedFences: len(cp.Fences), RetainedRecords: len(cp.Entries), LimitRecords: n.cfg.MaxRecords, ObservedAt: cp.CapturedAt, Peers: []subscription.PeerStatus{}, SyncSemantics: "process-local peer traversal receipts; completion does not prove global completeness or current origin authority"}
	if policy != nil {
		out.Mode = "partial"
		if policy.Regional() {
			out.SyncSemantics += "; fixed regional storage; unknown locations retained; authenticated out-of-area originals fetched transiently; local fences are not source proofs"
		}
	}
	for _, entry := range cp.Entries {
		if policy.Regional() && !entry.Component.Tombstone && policy.Selection.Region.Classify(entry.Component).Class == "unlocated" {
			out.UnknownLocationRecords++
		}
		if entry.Component.Tombstone {
			out.RetainedTombstones++
		}
		if !cp.CapturedAt.Before(entry.ExpiresAt) {
			out.ExpiredRecords++
		}
	}
	n.replicaStatusMu.Lock()
	keys := make([]string, 0, len(n.replicaPeers))
	for key := range n.replicaPeers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		peer := n.replicaPeers[key]

		out.Peers = append(out.Peers, peer)
	}
	n.replicaStatusMu.Unlock()
	writeJSON(w, 200, out)
}
