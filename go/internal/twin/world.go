package twin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/gateway"
)

// WorldOptions enables the separately configured signed simulation service in
// this twin process. An empty ConfigPath keeps the existing twin behavior.
// Ready receives the actual World address after initialization and must not
// block. It does not certify the presence of observations or an executor.
type WorldOptions struct {
	ConfigPath string
	Ready      func(string)
}

// ServeWithWorld owns listener and the shared lifetime of room mapping and the
// optional World service. World keeps its configured listener, signed paths,
// identities and storage. Existing room and physical robot APIs are unchanged;
// simulation World grants never enter their robot-control sessions.
func (s *Server) ServeWithWorld(ctx context.Context, listener net.Listener, options WorldOptions) error {
	if listener == nil {
		return errors.New("digital twin listener required")
	}
	defer listener.Close()
	if ctx == nil {
		return errors.New("digital twin context required")
	}
	if s.Store == nil {
		return errors.New("digital twin room store required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.Context = ctx
	var world *gateway.Service
	var worldDone <-chan struct{}
	if options.ConfigPath != "" {
		var err error
		world, err = gateway.StartWithOptions(ctx, options.ConfigPath, gateway.Options{StateBoundaries: []gateway.StateBoundary{{Directory: s.Store.directory, Subdirectory: "world"}}})
		if err != nil {
			return fmt.Errorf("start World service: %w", err)
		}
		defer world.Close()
		done := make(chan struct{})
		worldDone = done
		go func() { _ = world.Wait(); close(done) }()
		if options.Ready != nil {
			options.Ready(world.Address())
		}
	}
	server := &http.Server{Handler: s.Handler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	mappingDone := make(chan struct{})
	go func() { defer close(mappingDone); s.RunMapping() }()
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-httpDone:
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	case <-worldDone:
		result = world.Wait()
		if result == nil && ctx.Err() == nil {
			result = errors.New("World service stopped unexpectedly")
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		result = errors.Join(result, err)
	}
	<-mappingDone
	if world != nil {
		result = errors.Join(result, world.Close())
	}
	return result
}
