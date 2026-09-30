package cloudenroll

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Cloud reserves the device name and hands out the EAB secret exactly once,
// before the agent has redeemed it. If the agent step fails, a re-run cannot
// mint again (the name is held) and cloud cannot return the secret, so the
// enrollment is stranded. Keeping the minted credential until the agent
// redeems it lets the same command finish on a re-run.

// PendingPath is where the unredeemed credential for name in cfg's tenant
// directory is kept. The directory URL carries the tenant and the PKI host,
// so the same name in another tenant or deployment is a different entry.
func PendingPath(cfg acmeenroll.Config, name string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(cfg.DirectoryURL + "\x00" + name))
	return filepath.Join(dir, "pending-enrollments", hex.EncodeToString(sum[:16])+".json"), nil
}

// LoadPending returns the credential minted for name by an earlier run that
// the agent never redeemed, or ok=false when there is none.
func LoadPending(cfg acmeenroll.Config, name string) (acmeenroll.Config, bool, error) {
	path, err := PendingPath(cfg, name)
	if err != nil {
		return cfg, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, fmt.Errorf("reading pending enrollment: %w", err)
	}
	var pending acmeenroll.Config
	if err := json.Unmarshal(data, &pending); err != nil {
		return cfg, false, fmt.Errorf("pending enrollment %s is unreadable; delete it to enroll with a new credential: %w", path, err)
	}
	if pending.DirectoryURL != cfg.DirectoryURL {
		return cfg, false, nil
	}
	if err := pending.Validate(); err != nil {
		return cfg, false, fmt.Errorf("pending enrollment %s is invalid; delete it to enroll with a new credential: %w", path, err)
	}
	return pending, true, nil
}

// SavePending keeps cfg (which carries the EAB secret) owner-readable only.
func SavePending(cfg acmeenroll.Config, name string) error {
	path, err := PendingPath(cfg, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ClearPending forgets the credential once the agent has redeemed it.
func ClearPending(cfg acmeenroll.Config, name string) error {
	path, err := PendingPath(cfg, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
