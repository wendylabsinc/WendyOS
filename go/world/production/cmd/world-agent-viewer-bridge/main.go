package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/viewerbridge"
)

func main() { os.Exit(run()) }
func run() int {
	f := flag.NewFlagSet("world-agent-viewer-bridge", flag.ContinueOnError)
	path := f.String("config", "", "absolute simulation viewer configuration")
	if f.Parse(os.Args[1:]) != nil || f.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := viewerbridge.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "viewer bridge configuration refused")
		return 2
	}
	conn, err := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "explicit World service socket required")
		return 2
	}
	defer conn.Close()
	bridge, err := viewerbridge.NewHTTP(cfg, appv1.NewWorldAppServiceClient(conn))
	if err != nil {
		fmt.Fprintln(os.Stderr, "viewer bridge initialization refused")
		return 1
	}
	defer bridge.Close()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configured viewer listener unavailable")
		return 1
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := commandContext(ctx, cfg.Reader)
	defer cancel()
	server := &http.Server{Handler: bridge.Handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: time.Duration(cfg.RequestTimeoutMS)*time.Millisecond + time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 48 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "listening", "mode": "native_inference", "address": listener.Addr().String(), "run_id": cfg.Reader.People.RunID, "published": false, "media_available": false, "native_evidence_enabled": len(cfg.EvidenceSources) > 0}); err != nil {
		_ = server.Close()
		<-done
		return 1
	}
	select {
	case err = <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "viewer HTTP server stopped")
			return 1
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = server.Shutdown(shutdown)
		cancel()
		if err != nil {
			_ = server.Close()
		}
		<-done
	}
	return 0
}

// Continuous readers keep their original identity and version history until
// signal cancellation. Their individual native and HTTP requests stay bounded.
func commandContext(parent context.Context, cfg viewerbridge.Config) (context.Context, context.CancelFunc) {
	if cfg.Continuous {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(cfg.People.DurationMS)*time.Millisecond)
}
