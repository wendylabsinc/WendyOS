package ipcam

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// LoadDHCPAllowlist loads an explicit list of interfaces allowed to serve
// camera DHCP. A missing file means no interfaces are allowed. An invalid or
// insecure file also fails closed. Run rereads it on every link scan, so
// removing an opt-in withdraws an active camera DHCP server on the next scan.
// Camera discovery and passive DHCP observation do not depend on this list.
func (m *LinkManager) LoadDHCPAllowlist(path string) error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	m.allowlistPath = path
	m.allowedDHCP = make(map[string]bool)
	m.mu.Unlock()
	err := m.refreshDHCPAllowlist()
	m.withdrawRevokedClaims()
	return err
}

func (m *LinkManager) refreshDHCPAllowlist() error {
	m.mu.Lock()
	path := m.allowlistPath
	reader := m.readAllowlist
	m.mu.Unlock()
	if path == "" {
		return nil
	}
	allowed, err := reader(path)
	if err != nil {
		allowed = make(map[string]bool)
	}
	m.mu.Lock()
	m.allowedDHCP = allowed
	m.mu.Unlock()
	return err
}

func readDHCPAllowlist(path string) (map[string]bool, error) {
	allowed := make(map[string]bool)
	parent, err := os.Lstat(filepath.Dir(path))
	if os.IsNotExist(err) {
		return allowed, nil
	}
	if err != nil {
		return nil, fmt.Errorf("camera DHCP allowlist parent: %w", err)
	}
	if err := rootOwnedUnwritable(parent, true); err != nil {
		return nil, fmt.Errorf("camera DHCP allowlist parent: %w", err)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return allowed, nil
	}
	if err != nil {
		return nil, fmt.Errorf("camera DHCP allowlist: %w", err)
	}
	if err := rootOwnedUnwritable(info, false); err != nil {
		return nil, fmt.Errorf("camera DHCP allowlist: %w", err)
	}
	if info.Size() > 4096 {
		return nil, fmt.Errorf("camera DHCP allowlist exceeds 4096 bytes")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening camera DHCP allowlist: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, fmt.Errorf("reading camera DHCP allowlist: %w", err)
	}
	if len(data) > 4096 {
		return nil, fmt.Errorf("camera DHCP allowlist exceeds 4096 bytes")
	}
	return parseDHCPAllowlist(string(data))
}

func rootOwnedUnwritable(info os.FileInfo, directory bool) error {
	if directory && !info.IsDir() {
		return fmt.Errorf("must be a directory, without symlinks")
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular file, without symlinks")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("must be owned by root")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("must not be writable by group or others")
	}
	return nil
}

func parseDHCPAllowlist(contents string) (map[string]bool, error) {
	allowed := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for line := 1; scanner.Scan(); line++ {
		name := strings.TrimSpace(scanner.Text())
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if !validDHCPInterfaceName(name) {
			return nil, fmt.Errorf("invalid camera DHCP interface on line %d", line)
		}
		allowed[name] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading camera DHCP interfaces: %w", err)
	}
	return allowed, nil
}

func validDHCPInterfaceName(name string) bool {
	return len(name) > 0 && len(name) <= 15 && name != "." && name != ".." && !strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	})
}

// writeDHCPAllowlist durably replaces the agent-owned configuration. Errors
// after rename can leave the new file visible but its durability uncertain.
func writeDHCPAllowlist(path string, allowed map[string]bool) error {
	return writeDHCPAllowlistWithSync(path, allowed, syncDHCPDirectory)
}

func writeDHCPAllowlistWithSync(path string, allowed map[string]bool, syncDir func(string) error) error {
	if err := makeDHCPDirectory(filepath.Dir(path), syncDir); err != nil {
		return err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := rootOwnedUnwritable(parent, true); err != nil {
		return err
	}
	var names []string
	for name, enabled := range allowed {
		if !validDHCPInterfaceName(name) {
			return fmt.Errorf("invalid camera DHCP interface %q", name)
		}
		if enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	data := strings.Join(names, "\n") + "\n"
	if len(data) > 4096 {
		return fmt.Errorf("camera DHCP allowlist exceeds 4096 bytes")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".camera-dhcp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := rootOwnedUnwritable(info, false); err != nil {
		return err
	}
	if _, err := f.WriteString(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDHCPDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// A first enable may create the configuration directory. Sync its ancestors
// within the same filesystem, including on retry after a failed sync: an
// existing directory entry alone does not prove that it reached disk. Stop at
// mount boundaries; WendyOS binds an already provisioned persistent directory
// beneath its read-only root filesystem.
func makeDHCPDirectory(path string, syncDir func(string) error) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	var parents []string
	for child := filepath.Clean(path); filepath.Dir(child) != child; child = filepath.Dir(child) {
		childInfo, err := os.Stat(child)
		if err != nil {
			return err
		}
		parent := filepath.Dir(child)
		parentInfo, err := os.Stat(parent)
		if err != nil {
			return err
		}
		childStat, childOK := childInfo.Sys().(*syscall.Stat_t)
		parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
		if !childOK || !parentOK {
			return fmt.Errorf("cannot inspect configuration filesystem")
		}
		if childStat.Dev != parentStat.Dev {
			break
		}
		parents = append(parents, parent)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		if err := syncDir(parents[i]); err != nil {
			return err
		}
	}
	return nil
}
