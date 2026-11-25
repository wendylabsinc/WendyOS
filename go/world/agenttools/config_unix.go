//go:build !windows

package agenttools

import (
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"golang.org/x/sys/unix"
	"io"
	"os"
)

func loadConfig(path string) (client.Config, error) {
	var cfg client.Config
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return cfg, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), "world-agent-profile")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > authority.MaxEnvelopeBytes {
		return cfg, ErrUnavailable
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Uid != uint32(os.Getuid()) {
		return cfg, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil || authority.DecodeStrict(raw, &cfg) != nil {
		return cfg, ErrUnavailable
	}
	return cfg, nil
}
