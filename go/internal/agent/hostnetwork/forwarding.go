package hostnetwork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Forwarding is shared by mesh routing and physical-LAN projections. Closing
// either user must not turn off the other user's existing, scoped grants.
var forwarding = struct {
	sync.Mutex
	users map[string]bool
}{users: make(map[string]bool)}

var forwardingSysctl = "/proc/sys/net/ipv4/ip_forward"
var forwardingMarker = "/run/wendy-lan-forwarding/previous"

const forwardingComment = "wendy-scoped-forwarding-v1"

func forwardingRules() [][]string {
	return [][]string{
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
// kernel switch. HostPolicy places its wlmp isolation above this fallback.
func AcquireForwarding(key string) error {
	forwarding.Lock()
	defer forwarding.Unlock()
	if key == "" {
		return errors.New("empty forwarding consumer")
	}
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
	forwarding.users[key] = true
	return nil
}

func restoreForwarding() error {
	owned, err := forwardingBaseline()
	if err != nil || !owned {
		return err
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
	return os.Remove(forwardingMarker)
}

func ReleaseForwarding(key string) error {
	forwarding.Lock()
	defer forwarding.Unlock()
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
	return restoreForwarding()
}

func (a LANServiceAccess) forwardingKey() string {
	return fmt.Sprintf("lan:%s:%s:%s:%s:%s:%s:%d", a.NetNS, a.AppIP, a.AppBridge, a.Interface, a.Destination, a.Protocol, a.Port)
}
