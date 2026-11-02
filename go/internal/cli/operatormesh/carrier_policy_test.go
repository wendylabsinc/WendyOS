package operatormesh

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

func TestPinnedCarrierRetriesWithoutStartingDiscovery(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "11111111-2222-4111-8111-111111111111")
	peer, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	for _, transport := range []string{carriers.TCP, carriers.BLE} {
		t.Run(transport, func(t *testing.T) {
			s := &Session{Node: &localmesh.Node{Credentials: &localmesh.Credentials{Device: self}}, routerConfig: meshoperatorlink.ClientConfig{Router: peer}, options: Options{Transport: transport, RouterAddress: "127.0.0.1:43024"}}
			cfg := s.carrierConfig()
			// Check before running the real manager: a regression must fail here,
			// rather than opening a real native radio/network discovery provider.
			if cfg.Sources == nil || len(cfg.Sources) != 0 || len(cfg.Seeds) != 1 || cfg.Seeds[0].Transport != transport || cfg.Seeds[0].Device != peer {
				t.Fatalf("pinned session enabled discovery or lost its seed: %+v", cfg)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cfg.Snapshot = nil
			cfg.Limits.RetryDelay = time.Millisecond
			cfg.Limits.PollInterval = time.Millisecond
			var attempts atomic.Int32
			cfg.Connect = func(context.Context, carriers.Candidate) error {
				if attempts.Add(1) == 2 {
					cancel()
				}
				return errors.New("first hop temporarily unavailable")
			}
			if err := carriers.Run(ctx, cfg); !errors.Is(err, context.Canceled) || attempts.Load() != 2 {
				t.Fatalf("pinned seed did not retry independently: attempts=%d err=%v", attempts.Load(), err)
			}
		})
	}
}

func TestAutomaticAndExplicitCarrierSourcesRemainAvailable(t *testing.T) {
	s := &Session{Node: &localmesh.Node{Credentials: &localmesh.Credentials{}}, options: Options{Automatic: true}}
	if s.carrierConfig().Sources != nil {
		t.Fatal("automatic mode lost native discovery")
	}
	var observed bool
	s.options = Options{CarrierSources: []carriers.Source{func(context.Context, func([]carriers.Candidate)) error { observed = true; return nil }}}
	cfg := s.carrierConfig()
	if len(cfg.Sources) != 1 {
		t.Fatal("explicit fixture/provider sources were discarded")
	}
	if err := cfg.Sources[0](context.Background(), func([]carriers.Candidate) {}); err != nil || !observed {
		t.Fatal("explicit source did not run")
	}
}
