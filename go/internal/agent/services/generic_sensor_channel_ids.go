package services

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

const genericChannelIDBlockSize = 32

// genericChannelIDs reserves blocks for one device boot. The stored value is
// the last reserved ID, including IDs the current agent has yet to give out.
// The feed's mutex serializes calls to allocate.
type genericChannelIDs struct {
	path            string
	lastIssued      uint32
	reservedThrough uint32
}

func (a *genericChannelIDs) allocate() (uint32, error) {
	if a.lastIssued == a.reservedThrough {
		if err := a.reserve(); err != nil {
			return 0, err
		}
	}
	a.lastIssued++
	return a.lastIssued, nil
}

func (a *genericChannelIDs) reserve() error {
	if a.reservedThrough == 0 {
		data, err := os.ReadFile(a.path)
		switch {
		case err == nil:
			high, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
			if err != nil || high == 0 {
				return fmt.Errorf("invalid sensor channel ID counter in %s", a.path)
			}
			a.lastIssued = uint32(high)
			a.reservedThrough = uint32(high)
		case errors.Is(err, os.ErrNotExist):
			if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
				return fmt.Errorf("create sensor channel ID counter directory: %w", err)
			}
		default:
			return fmt.Errorf("read sensor channel ID counter: %w", err)
		}
	}
	if a.reservedThrough == math.MaxUint32 {
		return errors.New("sensor channel IDs exhausted for this boot")
	}
	high := min(uint64(a.reservedThrough)+genericChannelIDBlockSize, math.MaxUint32)
	// Production stores this file on /run (tmpfs). Replace it before making
	// any IDs in the block visible in a manifest.
	if err := atomicfile.Write(a.path, strconv.AppendUint(nil, high, 10), 0o600); err != nil {
		return fmt.Errorf("reserve sensor channel IDs: %w", err)
	}
	a.reservedThrough = uint32(high)
	return nil
}
