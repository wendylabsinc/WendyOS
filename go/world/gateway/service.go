package gateway

import (
	"context"
	"errors"
)

// Service owns one configured simulation node. Address reports its bound TCP
// listener; neither startup nor a healthy listener authorizes physical work.
type Service struct {
	address string
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

// Start opens the existing signed node configuration, durable stores and HTTP
// listener. It returns only after startup completes or every partial resource
// has been closed. It does not generate credentials or enable simulation for
// a configuration that did not explicitly permit it.
func Start(ctx context.Context, configPath string) (*Service, error) {
	return StartWithOptions(ctx, configPath, Options{})
}

// StateBoundary reserves a host's flat storage directory. World may use only
// the named dedicated child or paths outside the host directory. Existing
// direct files remain protected against symlink and hard-link aliases.
type StateBoundary struct{ Directory, Subdirectory string }
type Options struct{ StateBoundaries []StateBoundary }

func StartWithOptions(ctx context.Context, configPath string, options Options) (*Service, error) {
	if ctx == nil {
		return nil, errors.New("World gateway requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options.StateBoundaries = append([]StateBoundary(nil), options.StateBoundaries...)
	life, cancel := context.WithCancel(ctx)
	service := &Service{cancel: cancel, done: make(chan struct{})}
	ready := make(chan string, 1)
	go func() {
		service.err = serve(life, configPath, options, func(address string) { ready <- address })
		cancel()
		close(service.done)
	}()
	select {
	case service.address = <-ready:
		if err := ctx.Err(); err != nil {
			service.Close()
			return nil, err
		}
		return service, nil
	case <-service.done:
		if service.err == nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("World gateway closed during startup")
		}
		return nil, service.err
	case <-ctx.Done():
		service.Close()
		return nil, ctx.Err()
	}
}

func (s *Service) Address() string { return s.address }

// Wait returns after HTTP handlers, peer workers and durable stores close.
func (s *Service) Wait() error { <-s.done; return s.err }

// Close is concurrent-safe and idempotent. It cancels all owned workers before
// releasing their stores; it leaves the persisted state for the next startup.
func (s *Service) Close() error { s.cancel(); return s.Wait() }

func Run(ctx context.Context, configPath string) error {
	service, err := Start(ctx, configPath)
	if err != nil {
		return err
	}
	return service.Wait()
}
