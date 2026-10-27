package hostnetwork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Forwarding is shared by mesh routing and physical-LAN projections. Closing
// either user must not turn off the other user's existing, scoped grants.
var forwarding = struct {
	sync.Mutex
	users     map[string]bool
	lanOwners map[string]string // consumer key to the app IP whose grants it owns
}{users: make(map[string]bool)}

var forwardingSysctl = "/proc/sys/net/ipv4/ip_forward"
var forwardingMarker = "/run/wendy-lan-forwarding/previous"

const forwardingComment = "wendy-scoped-forwarding-v1"

// CNI invocations run in separate processes. Keep their grant changes and the
// agent's switch restoration in one critical section. Never replace this inode.
func withForwardingStateLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(forwardingMarker), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(forwardingMarker+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// A pending restoration means the agent's last user has left, but CNI grants
// still need forwarding. New agent users cancel it; final successful CNI DEL
// completes it. Unlike a PID, the exact kernel grant survives agent restart.
func pendingForwardingPath() string { return forwardingMarker + ".internet" }

func forwardingRules() [][]string {
	return [][]string{
		// LAN's per-app reply guard also covers public Internet replies on
		// that uplink. Let exact Internet grants inspect them first; their
		// private/service exclusions still fall through to scoped LAN grants.
		{"-m", "comment", "--comment", forwardingComment, "-j", BridgeInternetChain},
		{"-m", "comment", "--comment", forwardingComment, "-j", LANChainName},
		{"-m", "comment", "--comment", forwardingComment, "-j", "DROP"},
	}
}

func forwardingBaseline() (bool, error) {
	b, err := os.ReadFile(forwardingMarker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(b) != "0\n" {
		return false, errors.New("invalid owned forwarding baseline")
	}
	return true, nil
}

// AcquireForwarding is idempotent for a consumer key. The dedicated fallback
// preserves a host's disabled nonmesh forwarding policy before enabling the
// kernel switch. Internet sharing places its tunnel grants above this fallback.
func AcquireForwarding(key string) error {
	forwarding.Lock()
	defer forwarding.Unlock()
	if key == "" {
		return errors.New("empty forwarding consumer")
	}
	return withForwardingStateLock(func() error {
		if err := enableScopedForwarding(); err != nil {
			return err
		}
		if err := os.Remove(pendingForwardingPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		forwarding.users[key] = true
		return nil
	})
}

func enableScopedForwarding() error {
	owned, err := forwardingBaseline()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(forwardingSysctl)
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(b))
	if current != "0" && current != "1" {
		return errors.New("invalid IPv4 forwarding state")
	}
	if current == "0" && !owned {
		if err := os.MkdirAll(filepath.Dir(forwardingMarker), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(forwardingMarker, []byte("0\n"), 0600); err != nil {
			return err
		}
		owned = true
	}
	if owned {
		if err := InitLANServiceChain(); err != nil {
			return err
		}
		// Keep chain creation on the same command runner as the owned
		// forwarding rules, including in isolated unit-test fixtures.
		out, err := lanIPTables("-t", "filter", "-N", BridgeInternetChain)
		if err != nil && !strings.Contains(string(out), "already exists") {
			return fmt.Errorf("create Internet forwarding chain: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		rules := forwardingRules()
		for i := len(rules) - 1; i >= 0; i-- {
			if err := lanEnsureRule("filter", "FORWARD", "-I", rules[i]); err != nil {
				return err
			}
		}
		if current != "1" {
			if err := os.WriteFile(forwardingSysctl, []byte("1\n"), 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

func restoreForwarding() error {
	owned, err := forwardingBaseline()
	if err != nil || !owned {
		return err
	}
	active, err := bridgeInternetForwardingActive()
	if err != nil {
		return err
	}
	if active {
		return os.WriteFile(pendingForwardingPath(), []byte("1\n"), 0600)
	}
	// Disable first. Removing a fallback while forwarding is still enabled
	// would briefly expose unrelated CNI or foreign forwarding rules.
	if err := os.WriteFile(forwardingSysctl, []byte("0\n"), 0600); err != nil {
		return err
	}
	for _, rule := range forwardingRules() {
		if err := lanRemoveRule("filter", "FORWARD", rule); err != nil {
			return err
		}
	}
	if err := os.Remove(forwardingMarker); err != nil {
		return err
	}
	if err := os.Remove(pendingForwardingPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func ReleaseForwarding(key string) error {
	forwarding.Lock()
	defer forwarding.Unlock()
	return withForwardingStateLock(func() error { return releaseForwardingLocked(key) })
}

func releaseForwardingLocked(key string) error {
	if !forwarding.users[key] {
		if len(forwarding.users) == 0 {
			return restoreForwarding()
		}
		return nil
	}
	if len(forwarding.users) == 1 {
		if err := restoreForwarding(); err != nil {
			return err // retain the final consumer so cleanup can retry
		}
	}
	delete(forwarding.users, key)
	delete(forwarding.lanOwners, key)
	return nil
}

// Called after inherited grants have been revoked on agent startup. A crashed
// process's switch and guard are reclaimed only when no current user exists.
func reconcileUnusedForwarding() error {
	forwarding.Lock()
	defer forwarding.Unlock()
	if len(forwarding.users) != 0 {
		return nil
	}
	return withForwardingStateLock(restoreForwarding)
}

func (a LANServiceAccess) forwardingKey() string {
	return fmt.Sprintf("lan:%s:%s:%s:%s:%s:%s:%d", a.NetNS, a.AppIP, a.AppBridge, a.Interface, a.Destination, a.Protocol, a.Port)
}

// The app IP belongs to the CNI sandbox, while its namespace handle can change
// across bridge retries. Retain that scope separately from the consumer key so
// orphan cleanup can release the old handle's forwarding ownership too.
func acquireLANForwarding(a LANServiceAccess) error {
	key := a.forwardingKey()
	if err := AcquireForwarding(key); err != nil {
		return err
	}
	forwarding.Lock()
	defer forwarding.Unlock()
	if forwarding.lanOwners == nil {
		forwarding.lanOwners = make(map[string]string)
	}
	forwarding.lanOwners[key] = a.AppIP
	return nil
}

// The caller holds lanGuardMu and has revoked every scoped grant and route.
func releaseOrphanLANForwarding(appIP string) error {
	forwarding.Lock()
	var keys []string
	for key, owner := range forwarding.lanOwners {
		if !forwarding.users[key] {
			delete(forwarding.lanOwners, key)
		} else if owner == appIP {
			keys = append(keys, key)
		}
	}
	forwarding.Unlock()
	var errs []error
	for _, key := range keys {
		errs = append(errs, ReleaseForwarding(key))
	}
	return errors.Join(errs...)
}
