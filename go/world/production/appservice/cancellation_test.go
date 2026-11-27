package appservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/metadata"
)

type cancellationKey struct{}
type cancellableValidation struct {
	Publisher
	entered  chan struct{}
	finished chan struct{}
}

func (p *cancellableValidation) Validate(ctx context.Context) error {
	meta, _ := metadata.FromIncomingContext(ctx)
	if ctx.Value(cancellationKey{}) == true || len(meta.Get("test-cancel-validation")) > 0 {
		close(p.entered)
		<-ctx.Done()
		close(p.finished)
		return ctx.Err()
	}
	return p.Publisher.Validate(ctx)
}

func TestCancelledAuthorityCheckDoesNotCloseOtherRPCsOrSameScopeEnsure(t *testing.T) {
	for _, operation := range []string{"rpc", "ensure"} {
		t.Run(operation, func(t *testing.T) {
			entered, finished := make(chan struct{}), make(chan struct{})
			f := setup(t, func(cfg *Config) {
				factory := cfg.Factory
				cfg.Factory = func(ctx context.Context, scope serviceorigin.Scope) (Publisher, error) {
					p, err := factory(ctx, scope)
					if err != nil {
						return nil, err
					}
					return &cancellableValidation{Publisher: p, entered: entered, finished: finished}, nil
				}
			})
			mount := f.mount(t, f.scope)
			client, _ := dial(t, mount.SocketPath)
			publish(t, client)
			base := context.Background()
			if operation == "rpc" {
				base = metadata.NewOutgoingContext(base, metadata.Pairs("test-cancel-validation", "true"))
			} else {
				base = context.WithValue(base, cancellationKey{}, true)
			}
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "rpc" {
					_, err = client.Publish(ctx, request())
				} else {
					_, err = f.manager.Ensure(ctx, f.scope)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("authority validation not entered")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("authority validation did not observe cancellation")
			}
			if err := <-done; err == nil {
				t.Fatal("cancelled request succeeded")
			}
			// The same mounted connection and signing epoch must remain usable.
			publish(t, client)
			if same := f.mount(t, f.scope); same != mount {
				t.Fatal("ordinary caller cancellation replaced shared service authority")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.accepted) != 2 || f.accepted[0].Record.Epoch != f.accepted[1].Record.Epoch {
				t.Fatal("cancelled caller invalidated the publisher used by unrelated requests")
			}
		})
	}
}

func TestSocketOwnerMustRemainAgentIdentity(t *testing.T) {
	f := setup(t, nil)
	cfg := f.config
	cfg.UID++
	if _, err := New(cfg); err == nil {
		t.Fatal("application-owned socket root accepted")
	}
}

func TestSocketRootRefusesWritableAncestorsBeforeCreation(t *testing.T) {
	f := setup(t, nil)
	parent := filepath.Join(f.config.Directory, "writable")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	cfg := f.config
	cfg.Directory = filepath.Join(parent, "sockets")
	if _, err := New(cfg); err == nil {
		t.Fatal("socket root created beneath an untrusted ancestor")
	}
	if _, err := os.Stat(cfg.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("untrusted path mutated before refusal", err)
	}
}
