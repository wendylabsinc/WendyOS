package cloudrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/protobuf/proto"
)

const presenceLeaseHistoryLimit = 64
const presenceLeaseBytesLimit = 32768

type savedPresenceLease struct {
	path string
	when time.Time
}

func (a *Agent) presenceLeaseHistoryDir() string {
	return filepath.Join(a.StateDir, "presence-leases-v1")
}

func presenceLeaseName(id string) string { return binding([]byte(id)) + ".pb" }

// History is a private, bounded cache of Cloud-signed responses, never a new
// authority. Loading it verifies the issuer, lifetime and both current keys.
func (a *Agent) savedPresenceLeases() ([]savedPresenceLease, error) {
	dir := a.presenceLeaseHistoryDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved []savedPresenceLease
	for _, entry := range entries {
		name := entry.Name()
		// Only our fixed-size hashed names participate in cache cleanup.
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || len(name) != 46 || !strings.HasSuffix(name, ".pb") {
			continue
		}
		if decoded, e := decode64(strings.TrimSuffix(name, ".pb")); e != nil || len(decoded) != 32 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		saved = append(saved, savedPresenceLease{filepath.Join(dir, name), info.ModTime()})
	}
	slices.SortFunc(saved, func(x, y savedPresenceLease) int {
		if n := y.when.Compare(x.when); n != 0 {
			return n
		}
		return strings.Compare(x.path, y.path)
	})
	return saved, nil
}

func trimPresenceLeases(saved []savedPresenceLease, keep string, limit int) ([]savedPresenceLease, error) {
	// Preserve the active lease even across a backwards wall-clock adjustment.
	slices.SortStableFunc(saved, func(x, y savedPresenceLease) int {
		if x.path == keep && y.path != keep {
			return -1
		}
		if y.path == keep && x.path != keep {
			return 1
		}
		return 0
	})
	for len(saved) > limit {
		old := saved[len(saved)-1]
		if err := os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		saved = saved[:len(saved)-1]
	}
	return saved, nil
}

func (a *Agent) archivePresenceLease(lease *pb.IssuePresenceLeaseResponse, c claims) error {
	raw, err := proto.Marshal(lease)
	if err != nil {
		return err
	}
	if len(raw) > presenceLeaseBytesLimit {
		return fmt.Errorf("Cloud presence lease exceeds saved-state limit")
	}
	dir := a.presenceLeaseHistoryDir()
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, presenceLeaseName(c.JTI))
	saved, err := a.savedPresenceLeases()
	if err != nil {
		return err
	}
	limit := presenceLeaseHistoryLimit - 1
	for _, s := range saved {
		if s.path == path {
			limit++
			break
		}
	}
	if _, err = trimPresenceLeases(saved, path, limit); err != nil {
		return err
	}
	return writeState(path, raw)
}

func readPresenceLease(path string) (*pb.IssuePresenceLeaseResponse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, presenceLeaseBytesLimit+1))
	if err != nil || len(raw) > presenceLeaseBytesLimit {
		return nil, fmt.Errorf("invalid saved Cloud presence lease")
	}
	var lease pb.IssuePresenceLeaseResponse
	if proto.Unmarshal(raw, &lease) != nil {
		return nil, fmt.Errorf("invalid saved Cloud presence lease")
	}
	return &lease, nil
}

func (a *Agent) restorePresenceLeases(ctx context.Context, current claims) error {
	saved, err := a.savedPresenceLeases()
	if err != nil {
		return err
	}
	saved, err = trimPresenceLeases(saved, filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(current.JTI)), presenceLeaseHistoryLimit)
	if err != nil {
		return err
	}
	// A signer refresh cannot make cache restoration wait once per saved file.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, s := range saved {
		lease, err := readPresenceLease(s.path)
		var c claims
		if err == nil {
			c, err = a.validateLease(ctx, lease)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || filepath.Base(s.path) != presenceLeaseName(c.JTI) {
			// Malformed, expired and other-identity entries cannot contribute
			// claims; discard them without logging artifacts or credentials.
			if e := os.Remove(s.path); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			continue
		}
		if c.Aud == current.Aud && c.Route == current.Route && c.Signing == current.Signing && c.Encryption == current.Encryption {
			a.remember(c)
		}
	}
	a.remember(current)
	return nil
}
