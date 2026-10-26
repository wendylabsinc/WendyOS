package containerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type nanUnmanagedRule struct{ path, content string }

type nanRuleManager struct {
	gate          chan struct{}
	reloadPending bool // Protected by gate; disk equality does not prove reload success.
}

var appNDIRules = newNANRuleManager()

func newNANRuleManager() *nanRuleManager {
	// On agent restart the files may remain from a failed previous preparation.
	return &nanRuleManager{gate: make(chan struct{}, 1), reloadPending: true}
}

// All apps share the wa* rule files, so serialize their complete write/reload
// transaction independently of each app's lifecycle lock. A waiter must not
// create its NDI before an earlier reload completes successfully.
func (m *nanRuleManager) ensure(parent context.Context, rules []nanUnmanagedRule, reload func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, rule := range rules {
		current, err := os.ReadFile(rule.path)
		if err == nil && string(current) == rule.content {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read NAN interface rule %s: %w", rule.path, err)
		}
		m.reloadPending = true
		if err := os.MkdirAll(filepath.Dir(rule.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(rule.path, []byte(rule.content), 0o644); err != nil {
			return err
		}
	}
	if !m.reloadPending {
		return nil
	}
	if err := reload(ctx); err != nil {
		return fmt.Errorf("reload manager for app NAN interfaces: %w", err)
	}
	m.reloadPending = false
	return nil
}
