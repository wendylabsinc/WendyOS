package containerd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	labelKeyNetworkIdentity  = "sh.wendy/network.identity"
	labelKeyNetworkSandboxIP = "sh.wendy/network.sandbox.ip"
)

func networkSandboxPath(containerID string) string {
	path, err := safeJoin(cniNetnsBindDir, containerID)
	if err != nil {
		return ""
	}
	return path
}

func networkSandboxResultPath(containerID string) string {
	path, err := safeJoin(cniNetnsBindDir, containerID+".cni.json")
	if err != nil {
		return ""
	}
	return path
}

// networkSandboxEligible deliberately limits retention to the configuration
// whose complete host and namespace state can be verified by CNI CHECK. Mesh
// and multi-service networking retain their existing full DEL+ADD lifecycle.
func networkSandboxEligible(serviceName string, entitlements []appconfig.Entitlement) bool {
	if serviceName != "" {
		return false
	}
	_, bridge := findBridgeEntitlement(entitlements)
	_, mesh := findMeshEntitlement(entitlements)
	return bridge && !mesh
}

// networkSandbox is a CNI-configured network namespace that outlives an
// individual container task. cleanup owns the namespace bind mount.
type networkSandbox struct {
	appID        string
	serviceName  string
	containerID  string
	identity     string
	path         string
	ip           string
	result       string
	isolation    string
	entitlements []appconfig.Entitlement
	cleanup      func()
	once         sync.Once
}

// networkOperation is a reference-counted keyed mutex. Reference accounting
// lets the client remove idle keys without racing a waiter that already chose
// the same lock, avoiding unbounded growth across changing app IDs.
type networkOperation struct {
	mu   sync.Mutex
	refs int
}

func (s *networkSandbox) close() {
	if s != nil && s.cleanup != nil {
		s.once.Do(s.cleanup)
	}
}

// networkIdentity deliberately includes the complete entitlement set, not
// only network entitlements. This is conservative: an entitlement-only change
// gives up reuse even when it would happen to leave CNI unchanged, ensuring a
// reused namespace can never outlive a security-policy change.
func networkIdentity(isolation string, entitlements []appconfig.Entitlement) string {
	payload, err := json.Marshal(struct {
		Isolation    string                  `json:"isolation"`
		Entitlements []appconfig.Entitlement `json:"entitlements"`
	}{isolation, entitlements})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// networkIdentityFromLabels verifies that persisted desired-state labels
// describe an eligible sandbox and have not drifted from their fingerprint.
// Container labels are external state, so callers must not trust the stored
// fingerprint without recomputing it from isolation and entitlements.
func networkIdentityFromLabels(labels map[string]string) (string, bool) {
	expected, eligible := desiredNetworkIdentityFromLabels(labels)
	return expected, eligible && labels[labelKeyNetworkIdentity] == expected
}

// desiredNetworkIdentityFromLabels derives the fingerprint exclusively from
// the policy-bearing labels. Unlike networkIdentityFromLabels it deliberately
// ignores the persisted fingerprint, so write paths can never copy an
// attacker-influenced identity value back into container metadata.
func desiredNetworkIdentityFromLabels(labels map[string]string) (string, bool) {
	appID, serviceName := labels[labelKeyAppID], labels[labelKeyServiceName]
	if appconfig.ValidateAppID(appID) != nil || (serviceName != "" && appconfig.ValidateServiceName(serviceName) != nil) {
		return "", false
	}
	entitlements := parseEntitlementsFromAnnotations(labels)
	if !networkSandboxEligible(serviceName, entitlements) {
		return "", false
	}
	expected := networkIdentity(labels[labelKeyIsolation], entitlements)
	return expected, expected != ""
}

func (c *Client) registerNetworkSandbox(s *networkSandbox) {
	if s == nil {
		return
	}
	c.networkSandboxesMu.Lock()
	old := c.networkSandboxes[s.containerID]
	c.networkSandboxes[s.containerID] = s
	c.networkSandboxesMu.Unlock()
	if old != nil && old != s {
		old.close()
	}
}

func (c *Client) lockNetworkOperation(containerID string) func() {
	// Lock ordering invariant: when an operation needs both locks, the keyed
	// network-operation lock is always acquired before c.mu. Never call this
	// helper while holding c.mu; reversing that order can deadlock StartContainer's
	// metadata commit against create/stop/delete.
	c.networkOpsMu.Lock()
	if c.networkOps == nil {
		c.networkOps = make(map[string]*networkOperation)
	}
	op := c.networkOps[containerID]
	if op == nil {
		op = &networkOperation{}
		c.networkOps[containerID] = op
	}
	op.refs++
	c.networkOpsMu.Unlock()
	op.mu.Lock()
	return func() {
		op.mu.Unlock()
		c.networkOpsMu.Lock()
		op.refs--
		if op.refs == 0 && c.networkOps[containerID] == op {
			delete(c.networkOps, containerID)
		}
		c.networkOpsMu.Unlock()
	}
}

// lockAdditionalNetworkOperations extends an already-held operation lock to a
// set of concrete container IDs. Group stop/delete calls begin with the bare
// app-ID lock, while per-service create/start calls use container IDs; holding
// both key spaces is therefore required to serialize a whole-app lifecycle
// operation against every member. Caller must hold heldKey's lock and must not
// hold c.mu. Callers for one group share heldKey; sorting makes their additional
// per-member acquisition order deterministic.
func (c *Client) lockAdditionalNetworkOperations(heldKey string, containerIDs []string) func() {
	unique := make(map[string]struct{}, len(containerIDs))
	for _, containerID := range containerIDs {
		if containerID != "" && containerID != heldKey {
			unique[containerID] = struct{}{}
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	unlocks := make([]func(), 0, len(keys))
	for _, key := range keys {
		unlocks = append(unlocks, c.lockNetworkOperation(key))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

func (c *Client) reusableNetworkSandbox(ctx context.Context, containerID, identity string) (*networkSandbox, bool) {
	if identity == "" {
		return nil, false
	}
	c.networkSandboxesMu.Lock()
	s := c.networkSandboxes[containerID]
	c.networkSandboxesMu.Unlock()
	if s == nil || s.identity != identity || !networkSandboxHealthy(s.path, s.ip) {
		return nil, false
	}
	if !networkSandboxChecksPassed(true, c.CNICheck(ctx, s.appID, s.containerID, s.path, s.result, true)) || !c.refreshBridgeDNS(s.containerID, s.appID) {
		return nil, false
	}
	return s, true
}

// networkSandboxChecksPassed keeps the independent kernel-health and CNI CHECK
// gates explicit. In particular, a healthy-looking namespace or persisted
// prevResult can never compensate for a failed plugin CHECK.
//
// SECURITY: the vendored bridge CHECK uses prevResult only as expected state;
// it reopens both namespaces and queries live bridge/veth, address, and route
// state, while vendored host-local CHECK reopens its allocation store. These
// live checks are pinned by SECURITY comments at both CmdCheck implementations
// and must remain mandatory on vendored-plugin upgrades.
func networkSandboxChecksPassed(healthOK bool, cniCheckErr error) bool {
	return healthOK && cniCheckErr == nil
}

// refreshBridgeDNS drops and reacquires this single-service bridge's listener.
// Rebinding proves the gateway address is still present and usable instead of
// trusting only the agent's in-memory reference bookkeeping.
func (c *Client) refreshBridgeDNS(containerID, appID string) bool {
	gw, err := meshGateway(appID)
	if err != nil {
		return false
	}
	c.releaseMeshDNS(containerID, appID)
	return c.ensureMeshDNS(containerID, gw)
}

func (c *Client) takeNetworkSandbox(containerID string) *networkSandbox {
	c.networkSandboxesMu.Lock()
	s := c.networkSandboxes[containerID]
	delete(c.networkSandboxes, containerID)
	c.networkSandboxesMu.Unlock()
	return s
}

// destroyNetworkSandbox performs the complete external-state teardown for a
// retained namespace. It is idempotent: ownership is removed from the map
// before side effects begin, so concurrent/duplicate stop and delete paths do
// not release IPAM, DNS, or mesh state twice.
func (c *Client) destroyNetworkSandbox(ctx context.Context, containerID string) bool {
	s := c.takeNetworkSandbox(containerID)
	if s == nil {
		return false
	}
	if err := c.CNIDel(ctx, s.appID, s.containerID, s.path); err != nil {
		c.logger.Warn("CNI DEL failed while releasing reusable network sandbox (non-fatal)",
			zap.String("container_id", s.containerID), zap.Error(err))
	}
	if needsGatewayDNS(s.isolation, s.entitlements) {
		c.releaseMeshDNS(s.containerID, s.appID)
	}
	c.teardownMeshEgress(s.entitlements, s.containerID, s.appID, s.ip)
	s.close()
	_ = os.Remove(networkSandboxResultPath(containerID))
	c.logger.Debug("Released reusable network sandbox", zap.String("container_id", s.containerID))
	return true
}

func writeNetworkSandboxResult(containerID, result string) error {
	path := networkSandboxResultPath(containerID)
	if path == "" || result == "" {
		return fmt.Errorf("invalid reusable network sandbox result")
	}
	if err := os.MkdirAll(cniNetnsBindDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cniNetnsBindDir, ".cni-result-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(result); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readNetworkSandboxResult(containerID string) (string, error) {
	b, err := os.ReadFile(networkSandboxResultPath(containerID))
	if err != nil {
		return "", err
	}
	if len(b) == 0 || len(b) > cniStdoutLimit || !json.Valid(b) {
		return "", fmt.Errorf("invalid persisted CNI result")
	}
	return string(b), nil
}

// recoverNetworkSandbox reconstructs ownership after an agent restart. Every
// persisted field is treated as untrusted and CNI CHECK must prove host-local
// IPAM, bridge/veth attachment, and namespace addresses/routes.
func (c *Client) recoverNetworkSandbox(ctx context.Context, ctr containerd.Container, identity string, labels map[string]string, spec *runtimespec.Spec) (*networkSandbox, bool) {
	c.networkSandboxesMu.Lock()
	alreadyTracked := c.networkSandboxes[ctr.ID()] != nil
	c.networkSandboxesMu.Unlock()
	if alreadyTracked {
		return nil, false
	}
	appID, serviceName := labels[labelKeyAppID], labels[labelKeyServiceName]
	entitlements := parseEntitlementsFromAnnotations(labels)
	expectedIdentity, eligible := networkIdentityFromLabels(labels)
	if !eligible || expectedIdentity != identity {
		return nil, false
	}
	path := networkSandboxPath(ctr.ID())
	ip := labels[labelKeyNetworkSandboxIP]
	if path == "" || net.ParseIP(ip) == nil || !runtimeSpecJoinsNetworkSandbox(spec, path) || !networkSandboxHealthy(path, ip) {
		return nil, false
	}
	result, err := readNetworkSandboxResult(ctr.ID())
	if err != nil || !networkSandboxChecksPassed(true, c.CNICheck(ctx, appID, ctr.ID(), path, result, true)) {
		return nil, false
	}
	if !c.refreshBridgeDNS(ctr.ID(), appID) {
		return nil, false
	}
	s := &networkSandbox{appID: appID, serviceName: serviceName, containerID: ctr.ID(), identity: identity, path: path, ip: ip, result: result, isolation: labels[labelKeyIsolation], entitlements: entitlements, cleanup: func() { cleanupNetworkSandboxPath(path) }}
	c.registerNetworkSandbox(s)
	c.logger.Info("Recovered reusable network sandbox", zap.String("container_id", ctr.ID()), zap.String("ip", ip))
	return s, true
}

// purgePersistedNetworkSandbox tears down restart-orphaned state that could
// not be proven reusable. It is intentionally restricted to the canonical
// owner-controlled bind path and valid persisted labels.
func (c *Client) purgePersistedNetworkSandbox(ctx context.Context, ctr containerd.Container, labels map[string]string, spec *runtimespec.Spec) bool {
	path := networkSandboxPath(ctr.ID())
	_, pathErr := os.Lstat(path)
	_, resultErr := os.Lstat(networkSandboxResultPath(ctr.ID()))
	if path == "" || (!runtimeSpecJoinsNetworkSandbox(spec, path) && pathErr != nil && resultErr != nil) {
		return false
	}
	appID := labels[labelKeyAppID]
	if appconfig.ValidateAppID(appID) != nil {
		appID, _, _ = ParseContainerName(ctr.ID())
	}
	if appconfig.ValidateAppID(appID) == nil {
		_ = c.CNIDel(ctx, appID, ctr.ID(), path)
		c.releaseMeshDNS(ctr.ID(), appID)
	}
	cleanupNetworkSandboxPath(path)
	_ = os.Remove(networkSandboxResultPath(ctr.ID()))
	return true
}

// persistNetworkNamespace updates the mutable container metadata so a later
// NewTask joins the retained sandbox. An empty path restores the OCI default
// of creating a fresh private network namespace. A verified non-empty
// identity may be preserved across an explicit stop so the next start can
// retain its newly-created namespace again; sandbox IP metadata is always
// removed when path is empty.
func (c *Client) persistNetworkNamespace(ctx context.Context, ctr containerd.Container, path, identity, ip string) error {
	if path != "" && (path != networkSandboxPath(ctr.ID()) || filepath.Dir(path) != cniNetnsBindDir || identity == "" || net.ParseIP(ip) == nil) {
		return fmt.Errorf("refusing to persist invalid reusable network namespace")
	}
	if path == "" && ip != "" {
		return fmt.Errorf("refusing to persist sandbox IP without a network namespace")
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return fmt.Errorf("reading stored OCI spec: %w", err)
	}
	if spec.Linux == nil {
		return fmt.Errorf("stored OCI spec has no Linux section")
	}
	found := false
	for i := range spec.Linux.Namespaces {
		if spec.Linux.Namespaces[i].Type == "network" {
			spec.Linux.Namespaces[i].Path = path
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("stored OCI spec has no network namespace")
	}
	encoded, err := typeurl.MarshalAny(spec)
	if err != nil {
		return fmt.Errorf("encoding stored OCI spec: %w", err)
	}
	return ctr.Update(ctx, func(_ context.Context, _ *containerd.Client, record *containers.Container) error {
		record.Spec = encoded
		if record.Labels == nil {
			record.Labels = make(map[string]string)
		}
		// Recompute from the authoritative isolation/entitlement policy at the
		// write boundary. The identity argument is only an intent/consistency
		// check; it is never copied into metadata.
		desiredIdentity, eligible := desiredNetworkIdentityFromLabels(record.Labels)
		if identity != "" && (!eligible || identity != desiredIdentity) {
			return fmt.Errorf("network identity changed while persisting reusable namespace")
		}
		if path == "" {
			delete(record.Labels, labelKeyNetworkSandboxIP)
			if identity == "" {
				delete(record.Labels, labelKeyNetworkIdentity)
			} else {
				record.Labels[labelKeyNetworkIdentity] = desiredIdentity
			}
		} else {
			record.Labels[labelKeyNetworkIdentity] = desiredIdentity
			record.Labels[labelKeyNetworkSandboxIP] = ip
		}
		return nil
	})
}

func specJoinsNetworkSandbox(spec *localoci.Spec, path string) bool {
	if spec == nil || spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "network" {
			return ns.Path == path
		}
	}
	return false
}

func runtimeSpecJoinsNetworkSandbox(spec *runtimespec.Spec, path string) bool {
	if spec == nil || spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == runtimespec.NetworkNamespace {
			return ns.Path == path
		}
	}
	return false
}

func runtimeSpecHasNetworkNamespacePath(spec *runtimespec.Spec) bool {
	if spec == nil || spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == runtimespec.NetworkNamespace {
			return ns.Path != ""
		}
	}
	return false
}
