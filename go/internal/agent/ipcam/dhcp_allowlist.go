package ipcam

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LoadDHCPAllowlist loads an explicit list of interfaces allowed to serve
// camera DHCP. A missing file means no interfaces are allowed. An invalid or
// insecure file also fails closed. Run rereads it on every link scan, so
// removing an opt-in withdraws an active camera DHCP server on the next scan.
// Camera discovery and passive DHCP observation do not depend on this list.
func (m *LinkManager) LoadDHCPAllowlist(path string) error {
	m.mu.Lock()
	m.allowlistPath = path
	m.allowedDHCP = make(map[string]bool)
	m.mu.Unlock()
	return m.refreshDHCPAllowlist()
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
		if len(name) > 15 || len(name) == 0 || strings.ContainsFunc(name, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
		}) {
			return nil, fmt.Errorf("invalid camera DHCP interface on line %d", line)
		}
		allowed[name] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading camera DHCP interfaces: %w", err)
	}
	return allowed, nil
}
