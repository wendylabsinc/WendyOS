package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// The setup follows real request time. During write-count qualification, each
// sample advances one millisecond without charging disk or scheduler delays
// against the unchanged 500ms lease. HTTP signatures use this same test clock.
type supervisionCheckpointClock struct {
	last       atomic.Int64
	controlled atomic.Bool
}

func (c *supervisionCheckpointClock) now() time.Time {
	for {
		prior := c.last.Load()
		next := prior + 1
		if !c.controlled.Load() {
			next = time.Now().UnixMilli()
		}
		if next <= prior {
			next = prior + 1
		}
		if c.last.CompareAndSwap(prior, next) {
			return time.UnixMilli(next)
		}
	}
}

func replayCommitCount(t *testing.T, metrics *runtimeobs.Collector) uint64 {
	t.Helper()
	for _, duration := range metrics.Snapshot().Durations {
		if duration.Name == "replay.checkpoint_commit" {
			return duration.Count
		}
	}
	t.Fatal("replay commit metric is missing")
	return 0
}

func TestHTTPBatchSupervisionUsesFiveDurableCheckpoints(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, traced := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t/trace=%t", batch, traced), func(t *testing.T) {
				metrics := runtimeobs.New()
				clock := &supervisionCheckpointClock{}
				now := clock.now
				start := now()
				f, _, ex, a, b, spec, epoch := supervisedFixtureWithSetup(t, func(f *fixture, server Credentials, cfg *tasking.Config) {
					replay, err := authority.OpenReplay(filepath.Join(t.TempDir(), "http-replay"), 128)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = replay.Close() })
					replay.SetMetrics(metrics)
					v, err := authority.NewVerifier(authority.Config{RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: "tenant", Site: "office", Domain: "simulation", Audience: f.audience, Now: now, Replay: replay})
					if err != nil {
						t.Fatal(err)
					}
					at := now()
					rev, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: at.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: at.Add(time.Minute).UnixMilli()}, f.root)
					if err != nil {
						t.Fatal(err)
					}
					if err = v.UpdateRevocations(rev); err != nil {
						t.Fatal(err)
					}
					f.v, f.n.verifier = v, v
					cfg.Now = now
					cfg.Monotonic = func() time.Duration { return now().Sub(start) }
					cfg.ValidateAuthority = MissionAuthority(v)
					cfg.ValidateAuthorityContext = MissionAuthorityContext(v)
					cfg.ValidateSupervisor = SupervisorAuthority(v, server.Grant)
					cfg.ValidateSupervisorContext = SupervisorAuthorityContext(v, server.Grant)
					if batch {
						cfg.ValidateSupervisorsContext = SupervisorAuthorityBatchContext(v, server.Grant)
					}
				})
				clock.controlled.Store(true)
				f.requestNow = now
				ca := challenge(t, f, a, spec)
				acknowledge(t, f, a, ca)
				cb := challenge(t, f, b, spec)
				acknowledge(t, f, b, cb)
				if traced {
					f.n.cfg.Trace = nodeTrace(t)
				}
				before := replayCommitCount(t, metrics)
				d := evaluateSupervised(t, f, ex, spec, epoch)
				got := replayCommitCount(t, metrics) - before
				want := uint64(11)
				if batch {
					want = 5
				}
				if got != want {
					t.Fatalf("actual replay writes = %d, want %d", got, want)
				}
				if d.Action != mission.Continue || d.Record.Status.Phase != mission.Running {
					t.Fatalf("authorized two-operator evaluation changed: %+v", d)
				}
				if traced {
					snapshot := f.n.cfg.Trace.Snapshot()
					if len(snapshot.Requests) != 1 || snapshot.Requests[0].Class != "evaluate" || snapshot.Requests[0].Status != http.StatusOK {
						t.Fatal("evaluation was not correlated", snapshot.Requests)
					}
					var commits uint64
					for _, event := range snapshot.Events {
						if event.Phase == "replay_commit" {
							if event.RequestID != snapshot.Requests[0].ID || event.Failed || event.EndNS < 0 {
								t.Fatal("invalid correlated commit", event)
							}
							commits++
						}
					}
					if commits != got {
						t.Fatalf("trace counted %d commits, durable metrics counted %d", commits, got)
					}
				}
			})
		}
	}
}

func TestHTTPBatchSupervisionSelectiveRevocationAndGatewayLoss(t *testing.T) {
	f, _, ex, a, b, spec, epoch := supervisedFixtureWithSetup(t, func(f *fixture, server Credentials, cfg *tasking.Config) {
		cfg.ValidateSupervisorsContext = SupervisorAuthorityBatchContext(f.v, server.Grant)
	})
	acknowledge(t, f, a, challenge(t, f, a, spec))
	acknowledge(t, f, b, challenge(t, f, b, spec))
	if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
		t.Fatal("two valid operators did not authorize motion", d)
	}
	revokeSupervisors(t, f, 2, a.Grant.Grant.Principal)
	if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
		t.Fatal("selective denial removed the remaining operator", d)
	}
	revokeSupervisors(t, f, 3, a.Grant.Grant.Principal, "gateway")
	if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
		t.Fatal("gateway denial failed to latch fallback", d)
	}
}

func TestHTTPBatchGlobalFailureCannotResumeAfterRecovery(t *testing.T) {
	for _, stage := range []int32{1, 2} {
		t.Run(fmt.Sprintf("stage=%d", stage), func(t *testing.T) {
			var armed atomic.Bool
			var calls atomic.Int32
			f, _, ex, a, b, spec, epoch := supervisedFixtureWithSetup(t, func(f *fixture, server Credentials, cfg *tasking.Config) {
				verify := SupervisorAuthorityBatchContext(f.v, server.Grant)
				cfg.ValidateSupervisorsContext = func(ctx context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
					if armed.Load() && calls.Add(1) == stage {
						return nil, authority.ErrStaleRevocations
					}
					return verify(ctx, proofs)
				}
			})
			acknowledge(t, f, a, challenge(t, f, a, spec))
			acknowledge(t, f, b, challenge(t, f, b, spec))
			if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Continue {
				t.Fatal(d)
			}
			armed.Store(true)
			code, body := call(t, f, ex, http.MethodPost, "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: telemetry()}))
			if code != http.StatusConflict || calls.Load() != stage {
				t.Fatalf("global failure was not reached/refused: status=%d calls=%d body=%s", code, calls.Load(), body)
			}
			armed.Store(false)
			retained, err := f.n.cfg.Tasks.Get(spec.ID)
			if err != nil || retained.Status.Phase != mission.Held {
				t.Fatalf("fallback was not retained: %+v %v", retained, err)
			}
			request := jsonBody(t, SupervisionChallengeRequest{TaskID: spec.ID, TaskDigest: tasking.SpecDigest(spec), RequestID: "fresh-request-after-recovery"})
			if code, body = call(t, f, b, http.MethodPost, "/v1/tasks/supervision/challenge", "supervise", request); code != http.StatusConflict {
				t.Fatalf("fresh authority resumed lost coverage: %d %s", code, body)
			}
			if d := evaluateSupervised(t, f, ex, spec, epoch); d.Action != mission.Hold || d.Record.Status.Phase != mission.Held {
				t.Fatal("policy recovery resumed motion", d)
			}
		})
	}
}
