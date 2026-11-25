// world-mcp serves only the fixed World agent's five bounded tools on stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/worldmcp"
	"golang.org/x/sys/unix"
)

var errStartup = errors.New("world-mcp startup refused; check the private agent configuration")

func loadConfig(path string) (client.Config, error) {
	var cfg client.Config
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return cfg, errStartup
	}
	f := os.NewFile(uintptr(fd), "agent-config")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > authority.MaxEnvelopeBytes {
		return cfg, errStartup
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Uid != uint32(os.Getuid()) {
		return cfg, errStartup
	}
	raw, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil || authority.DecodeStrict(raw, &cfg) != nil {
		return cfg, errStartup
	}
	return cfg, nil
}

type stdoutWriter struct{ io.Writer }

func (stdoutWriter) Close() error { return nil }

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("world-mcp", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("credentials", "", "fixed private agent configuration, mode 0600")
	if f.Parse(args) != nil || *path == "" || f.NArg() != 0 {
		return errStartup
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	server, err := worldmcp.New(cfg)
	if err != nil {
		return errStartup
	}
	return worldmcp.Serve(ctx, server, os.Stdin, stdoutWriter{os.Stdout})
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && ctx.Err() == nil {
		// Never include file paths, backend response bodies or SDK peer input.
		_, _ = io.WriteString(os.Stderr, "world-mcp stopped: configuration or protocol unavailable\n")
		os.Exit(1)
	}
}
