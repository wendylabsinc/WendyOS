package replicastore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testtrack"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type authorizationTraceClock struct {
	inner trustedtime.Provider
	calls int
}

func (c *authorizationTraceClock) Now() (trustedtime.Interval, error) {
	c.calls++
	return c.inner.Now()
}

func TestStoreAuthorizationTraceDistinguishesExistingRefusals(t *testing.T) {
	for _, mode := range []string{"visible", "peer", "source", "interpretation", "grant-window"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixtureComponent(t, resultrecord.ComponentType, "tracks")
			w := f.clock.w
			f.request = testtrack.Request(t, f.scope, f.a.cfg.Replica.Frames[0], "repair", time.Unix(0, f.sample.Load()), w.Earliest)
			f.lease(t)
			raw := f.publish(t)
			if _, err := f.syncService(t).SyncPeer(context.Background(), deviceA); err != nil {
				t.Fatal(err)
			}
			var signed serviceorigin.SignedRecord
			if err := json.Unmarshal(raw, &signed); err != nil {
				t.Fatal(err)
			}
			verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: tenant, Site: "office", ExecutionDomain: "simulation", Clock: f.clock, Certificates: f.b.cfg.Certificates,
				Authorize: func(context.Context, serviceorigin.DelegationClaims, wendycert.DeviceIdentity) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			v, err := verifier.Verify(context.Background(), signed)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			switch mode {
			case "peer":
				f.b.store.cfg.Authorize = func(context.Context, replication.Identity, uint64, replication.Direction, []proofref.Source, trustedtime.Interval) (servicejournal.AuthorityWindow, error) {
					return servicejournal.AuthorityWindow{}, ErrDenied
				}
				want = "remote_peer_grant_omitted"
			case "source":
				_, err = f.b.cfg.Authority.IssueFloor(context.Background(), sourceauthority.Signer{ChainDER: f.identity.ChainDER, DeviceKey: f.identity.Key}, sourceauthority.SourceOf(f.scope), 2)
				if err != nil {
					t.Fatal(err)
				}
				want = "remote_source_authority_omitted"
			case "interpretation":
				f.clock.advance(4 * time.Second)
				w = f.clock.w
				want = "remote_interpretation_omitted"
			case "grant-window":
				w.Earliest = w.Earliest.Add(-time.Millisecond)
				want = "remote_grant_window_omitted"
			}
			clock := &authorizationTraceClock{inner: f.b.store.cfg.Clock}
			f.b.store.cfg.Clock = clock
			run := func(ctx context.Context) (servicejournal.AuthorityWindow, error) {
				var window servicejournal.AuthorityWindow
				err := f.b.store.guard(ctx, func(ctx context.Context) error {
					var e error
					window, e = f.b.store.authorizeRecord(context.WithValue(ctx, snapshotContextKey{}, f.b.store), v, w)
					return e
				})
				return window, err
			}
			plain, plainErr := run(context.Background())
			plainCalls := clock.calls
			clock.calls = 0
			sink, err := rpctrace.New(rpctrace.Config{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, finish := rpctrace.Begin(context.Background(), sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "reader", Service: "reader", Generation: 1})
			got, gotErr := run(ctx)
			finish(rpctrace.OK)
			sink.Close()
			var output bytes.Buffer
			if err := sink.Run(context.Background(), &output); err != nil {
				t.Fatal(err)
			}
			var event rpctrace.Event
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if got != plain || !errors.Is(gotErr, plainErr) || clock.calls != plainCalls {
				t.Fatal("trace changed result or clock reads", got, gotErr, plain, plainErr, clock.calls, plainCalls)
			}
			if (want == "") != (gotErr == nil) {
				t.Fatal(mode, gotErr)
			}
			counts := map[string]uint64{}
			for _, stage := range event.Stages {
				if stage.Stage == "total" {
					continue
				}
				if stage.SumNS != 0 || stage.MaxNS != 0 {
					t.Fatal(stage)
				}
				counts[stage.Stage] = stage.Count
			}
			if want == "" && len(counts) != 0 || want != "" && (len(counts) != 1 || counts[want] != 1) {
				t.Fatal(counts, want)
			}
		})
	}
}
