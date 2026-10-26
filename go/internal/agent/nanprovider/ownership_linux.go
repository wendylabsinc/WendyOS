//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/agent/networkmanager"
	"golang.org/x/sys/unix"
)

type ownedRule struct {
	path, content string
	created       bool
}

// reserveNDI serializes this host's NAN provider and prevents network managers
// from taking over its NDI before its private underlay address is configured.
func reserveNDI(ctx context.Context) (func(), error) {
	rules := []ownedRule{
		{path: "/run/systemd/network/05-wendy-nan-provider.network", content: "# Wendy NAN provider v1\n[Match]\nName=wnanndi0\n[Link]\nUnmanaged=yes\n"},
		{path: "/run/NetworkManager/conf.d/05-wendy-nan-provider.conf", content: "# Wendy NAN provider v1\n[device-wendy-nan-provider]\nmatch-device=interface-name:wnanndi0\nmanaged=0\n"},
	}
	return reserveNDIRules(ctx, "/run/wendy-nan-provider.lock", rules, reloadNetworkManagers)
}

func reserveNDIRules(ctx context.Context, lockPath string, rules []ownedRule, reload func(context.Context) error) (func(), error) {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("NAN provider already running: %w", err), lock.Close())
	}
	cleanup := func() {
		for _, rule := range rules {
			if !rule.created {
				continue
			}
			if data, err := os.ReadFile(rule.path); err == nil && string(data) == rule.content {
				_ = os.Remove(rule.path)
			}
		}
		_ = reload(context.Background())
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}
	for i := range rules {
		rule := &rules[i]
		if err = os.MkdirAll(filepath.Dir(rule.path), 0755); err != nil {
			cleanup()
			return nil, err
		}
		file, createErr := os.OpenFile(rule.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if errors.Is(createErr, os.ErrExist) {
			data, readErr := os.ReadFile(rule.path)
			if readErr != nil || string(data) != rule.content {
				cleanup()
				return nil, fmt.Errorf("NAN network rule %s already owned by another component", rule.path)
			}
			// An identical rule left by a crashed provider belongs to this
			// exclusive session and is removed during normal shutdown.
			rule.created = true
			continue
		}
		if createErr != nil {
			cleanup()
			return nil, createErr
		}
		rule.created = true
		written, writeErr := file.WriteString(rule.content)
		// Cleanup owns exactly the bytes this attempt wrote, including a short
		// write. Keep preserving any rule changed by another host component.
		rule.content = rule.content[:written]
		err = errors.Join(writeErr, file.Close())
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	if err = reload(ctx); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func reloadNetworkManagers(ctx context.Context) error {
	return networkmanager.Reload(ctx)
}
