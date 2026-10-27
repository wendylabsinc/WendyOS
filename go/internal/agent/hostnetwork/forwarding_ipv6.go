package hostnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// IPv6 forwarding is shared by mesh transit and Internet tunnel endpoints.
// The process owns the switch only when it found global forwarding disabled.
// Each network namespace gets its own crash journal, even when /run is shared.
var ipv6Forwarding = struct {
	sync.Mutex
	users map[string]bool
}{users: map[string]bool{}}

var ipv6ForwardingWriteFile = os.WriteFile
var ipv6ForwardingConf = "/proc/sys/net/ipv6/conf"
var ipv6ForwardingInterfaces = net.Interfaces
var ipv6ForwardingJournal = func() (string, error) {
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(namespace, "net:[") || !strings.HasSuffix(namespace, "]") {
		return "", errors.New("invalid network namespace identity")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(namespace, "net:["), "]")
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", err
	}
	return filepath.Join("/run/wendy-ipv6-forwarding", id+".json"), nil
}
var ipv6ForwardingIPTables = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ip6tables", append([]string{"-w", "3", "-t", "filter"}, args...)...).CombinedOutput()
}

const ipv6ForwardingComment = "wendy-scoped-ipv6-forwarding"

type ipv6ForwardingFlags struct{ Forwarding, AcceptRA string }
type ipv6ForwardingInterface struct {
	Name  string
	Index int
	ipv6ForwardingFlags
}
type ipv6ForwardingBaseline struct {
	Version    int
	Global     string
	Default    ipv6ForwardingFlags
	Interfaces []ipv6ForwardingInterface
}

func readForwarding6(iface, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(ipv6ForwardingConf, iface, key))
	return strings.TrimSpace(string(b)), err
}
func writeForwarding6(iface, key, value string) error {
	return ipv6ForwardingWriteFile(filepath.Join(ipv6ForwardingConf, iface, key), []byte(value+"\n"), 0600)
}
func readForwarding6Flags(name string) (ipv6ForwardingFlags, error) {
	f, err := readForwarding6(name, "forwarding")
	if err != nil {
		return ipv6ForwardingFlags{}, err
	}
	ra, err := readForwarding6(name, "accept_ra")
	flags := ipv6ForwardingFlags{f, ra}
	if err == nil && !validForwarding6Flags(flags) {
		err = errors.New("invalid IPv6 forwarding or RA state")
	}
	return flags, err
}
func validForwarding6Flags(flags ipv6ForwardingFlags) bool {
	return (flags.Forwarding == "0" || flags.Forwarding == "1") && (flags.AcceptRA == "0" || flags.AcceptRA == "1" || flags.AcceptRA == "2")
}
func forwarding6RA(before string) string {
	if before == "1" {
		return "2"
	}
	return before
}

func loadForwarding6(path string) (*ipv6ForwardingBaseline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var baseline ipv6ForwardingBaseline
	if err := json.Unmarshal(data, &baseline); err != nil {
		return nil, err
	}
	if baseline.Version != 1 || baseline.Global != "0" || !validForwarding6Flags(baseline.Default) {
		return nil, errors.New("invalid owned IPv6 forwarding baseline")
	}
	seen := map[string]bool{}
	for _, entry := range baseline.Interfaces {
		key := fmt.Sprintf("%s/%d", entry.Name, entry.Index)
		if entry.Name == "" || entry.Name == "." || entry.Name == ".." || entry.Name == "all" || entry.Name == "default" || strings.ContainsAny(entry.Name, "/\\") || entry.Index <= 0 || !validForwarding6Flags(entry.ipv6ForwardingFlags) || seen[key] {
			return nil, errors.New("invalid saved IPv6 interface baseline")
		}
		seen[key] = true
	}
	return &baseline, nil
}

// Journal before changing any sysctl. fsync both the file and its directory so
// a power loss cannot leave enabled forwarding without its original baseline.
func saveForwarding6(path string, baseline *ipv6ForwardingBaseline) error {
	data, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ipv6-forwarding-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncForwarding6Directory(dir)
}
func syncForwarding6Directory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func forwarding6Guard(ensure bool) error {
	rule := []string{"-m", "comment", "--comment", ipv6ForwardingComment, "-j", "DROP"}
	out, err := ipv6ForwardingIPTables(append([]string{"-C", "FORWARD"}, rule...)...)
	if err != nil && exitCode(err) != 1 {
		return fmt.Errorf("inspect IPv6 forwarding guard: %w: %s", err, out)
	}
	if ensure && err != nil {
		out, err = ipv6ForwardingIPTables(append([]string{"-I", "FORWARD", "1"}, rule...)...)
	} else if !ensure && err == nil {
		out, err = ipv6ForwardingIPTables(append([]string{"-D", "FORWARD"}, rule...)...)
	} else {
		return nil
	}
	if err != nil {
		return fmt.Errorf("update IPv6 forwarding guard: %w: %s", err, out)
	}
	return nil
}

func captureForwarding6() (*ipv6ForwardingBaseline, error) {
	defaults, err := readForwarding6Flags("default")
	if err != nil {
		return nil, err
	}
	links, err := ipv6ForwardingInterfaces()
	if err != nil {
		return nil, err
	}
	baseline := &ipv6ForwardingBaseline{Version: 1, Global: "0", Default: defaults}
	for _, link := range links {
		flags, err := readForwarding6Flags(link.Name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		baseline.Interfaces = append(baseline.Interfaces, ipv6ForwardingInterface{link.Name, link.Index, flags})
	}
	return baseline, nil
}

// Interfaces created while forwarding is owned inherit the changed defaults.
// Record their original-default equivalent before restoration, preserving any
// value that has since diverged from the value we installed.
func extendForwarding6(path string, baseline *ipv6ForwardingBaseline, links []net.Interface) error {
	seen := map[string]bool{}
	for _, entry := range baseline.Interfaces {
		seen[fmt.Sprintf("%s/%d", entry.Name, entry.Index)] = true
	}
	changed := false
	for _, link := range links {
		if seen[fmt.Sprintf("%s/%d", link.Name, link.Index)] {
			continue
		}
		flags, err := readForwarding6Flags(link.Name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if flags.Forwarding == "1" {
			flags.Forwarding = baseline.Default.Forwarding
		}
		if flags.AcceptRA == forwarding6RA(baseline.Default.AcceptRA) {
			flags.AcceptRA = baseline.Default.AcceptRA
		}
		baseline.Interfaces = append(baseline.Interfaces, ipv6ForwardingInterface{link.Name, link.Index, flags})
		changed = true
	}
	if changed {
		return saveForwarding6(path, baseline)
	}
	return nil
}

func restoreForwarding6(path string, baseline *ipv6ForwardingBaseline) error {
	if baseline == nil {
		return nil
	}
	links, err := ipv6ForwardingInterfaces()
	if err != nil {
		return err
	}
	if err := extendForwarding6(path, baseline, links); err != nil {
		return err
	}
	// Keep the guard until all forwarding flags have been restored. Writing all
	// also overwrites default and existing per-interface flags, so restore it first.
	if err := forwarding6Guard(true); err != nil {
		return err
	}
	if err := writeForwarding6("all", "forwarding", baseline.Global); err != nil {
		return err
	}
	current := map[string]int{}
	for _, link := range links {
		current[link.Name] = link.Index
	}
	entries := append([]ipv6ForwardingInterface{{Name: "default", ipv6ForwardingFlags: baseline.Default}}, baseline.Interfaces...)
	var errs []error
	for _, entry := range entries {
		if entry.Name != "default" && current[entry.Name] != entry.Index {
			continue
		}
		ra, err := readForwarding6(entry.Name, "accept_ra")
		if err == nil && ra != entry.AcceptRA && ra != forwarding6RA(entry.AcceptRA) {
			err = fmt.Errorf("IPv6 RA setting changed externally on %s", entry.Name)
		}
		if err == nil {
			err = writeForwarding6(entry.Name, "accept_ra", entry.AcceptRA)
		}
		if err != nil {
			errs = append(errs, err)
		}
		if err := writeForwarding6(entry.Name, "forwarding", entry.Forwarding); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := forwarding6Guard(false); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncForwarding6Directory(filepath.Dir(path))
}

// ReconcileIPv6Forwarding restores a crashed process's baseline only when no
// live consumer exists. Call it before rebuilding scoped forwarding grants.
func ReconcileIPv6Forwarding() error {
	ipv6Forwarding.Lock()
	defer ipv6Forwarding.Unlock()
	if len(ipv6Forwarding.users) != 0 {
		return nil
	}
	path, err := ipv6ForwardingJournal()
	if err != nil {
		return err
	}
	baseline, err := loadForwarding6(path)
	if err != nil {
		return err
	}
	return restoreForwarding6(path, baseline)
}

// AcquireIPv6Forwarding is idempotent per consumer. Add each consumer's scoped
// FORWARD grants after acquisition; they must sit above the owned fallback.
func AcquireIPv6Forwarding(key string) (result error) {
	ipv6Forwarding.Lock()
	defer ipv6Forwarding.Unlock()
	if key == "" {
		return errors.New("empty IPv6 forwarding consumer")
	}
	path, err := ipv6ForwardingJournal()
	if err != nil {
		return err
	}
	baseline, err := loadForwarding6(path)
	if err != nil {
		return err
	}
	if baseline != nil && len(ipv6Forwarding.users) == 0 {
		if err := restoreForwarding6(path, baseline); err != nil {
			return err
		}
		baseline = nil
	}
	// A failed first acquire must not strand RA changes or a fallback guard
	// merely because the caller never obtained a lease. Leave the journal in
	// place if rollback itself fails so startup recovery can retry safely.
	if len(ipv6Forwarding.users) == 0 {
		defer func() {
			if result == nil {
				return
			}
			saved, err := loadForwarding6(path)
			if err == nil {
				err = restoreForwarding6(path, saved)
			}
			if err != nil {
				result = errors.Join(result, fmt.Errorf("restore failed IPv6 acquisition: %w", err))
			}
		}()
	}
	global, err := readForwarding6("all", "forwarding")
	if err != nil {
		return err
	}
	if global != "0" && global != "1" {
		return errors.New("invalid IPv6 global forwarding state")
	}
	if baseline == nil && global == "0" {
		baseline, err = captureForwarding6()
		if err != nil {
			return err
		}
		if err := saveForwarding6(path, baseline); err != nil {
			return err
		}
	}
	if baseline != nil {
		if err := forwarding6Guard(true); err != nil {
			return err
		}
		entries := append([]ipv6ForwardingInterface{{Name: "default", ipv6ForwardingFlags: baseline.Default}}, baseline.Interfaces...)
		links, err := ipv6ForwardingInterfaces()
		if err != nil {
			return err
		}
		current := map[string]int{}
		for _, link := range links {
			current[link.Name] = link.Index
		}
		for _, entry := range entries {
			if entry.Name != "default" && current[entry.Name] != entry.Index {
				continue
			}
			if entry.AcceptRA != "1" {
				continue
			}
			value, err := readForwarding6(entry.Name, "accept_ra")
			if err != nil {
				return err
			}
			if value != "1" && value != "2" {
				return fmt.Errorf("IPv6 RA setting changed externally on %s", entry.Name)
			}
			if err := writeForwarding6(entry.Name, "accept_ra", "2"); err != nil {
				return err
			}
		}
		if global != "1" {
			if err := writeForwarding6("all", "forwarding", "1"); err != nil {
				return err
			}
		}
	}
	ipv6Forwarding.users[key] = true
	return nil
}

// ReleaseIPv6Forwarding retains the final owner on cleanup failure, allowing a
// retry without another consumer accidentally removing the forwarding guard.
func ReleaseIPv6Forwarding(key string) error {
	ipv6Forwarding.Lock()
	defer ipv6Forwarding.Unlock()
	if len(ipv6Forwarding.users) > 1 || len(ipv6Forwarding.users) == 1 && !ipv6Forwarding.users[key] {
		delete(ipv6Forwarding.users, key)
		return nil
	}
	path, err := ipv6ForwardingJournal()
	if err != nil {
		return err
	}
	baseline, err := loadForwarding6(path)
	if err != nil {
		return err
	}
	if err := restoreForwarding6(path, baseline); err != nil {
		return err
	}
	delete(ipv6Forwarding.users, key)
	return nil
}
