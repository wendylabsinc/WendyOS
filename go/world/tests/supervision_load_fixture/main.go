// supervision_load_fixture publishes bounded, explicitly synthetic load records.
// It has no mission, stop, execution, enrollment or credential-issuance path.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

type options struct {
	credentials, trust, runID     string
	duration                      time.Duration
	entities, hz                  int
	preload, verifyPreload        bool
	count, nodeIndex, minFresh    int
	manifest, stateDir            string
	onsetVariant, release, report string
}

func readPrivate(path string, value any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("load input unavailable")
	}
	f := os.NewFile(uintptr(fd), "load-input")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("load input must be a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	defer clear(data)
	if err != nil || len(data) > authority.MaxEnvelopeBytes || authority.DecodeStrict(data, value) != nil {
		return errors.New("invalid bounded load input")
	}
	return nil
}

func load(o options) (client.Config, *client.Client, error) {
	var cfg client.Config
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(o.runID) || o.duration < time.Second || o.duration > 40*time.Second || o.entities < 1 || o.entities > 200 || o.hz < 1 || o.hz > 2 {
		return cfg, nil, errors.New("load requires explicit simulation run, 1..200 entities, 1..2 Hz and duration 1..40s")
	}
	if err := readPrivate(o.credentials, &cfg); err != nil {
		return cfg, nil, err
	}
	var trust client.SupervisionTrust
	if err := readPrivate(o.trust, &trust); err != nil {
		clear(cfg.PrivateKey)
		return client.Config{}, nil, err
	}
	g := cfg.Grant.Grant
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || !cfg.AllowInsecureSimulation || cfg.Audience != "node-2" || g.PublishAudience != "node-2" || g.Domain != "simulation" || g.Tenant != "lab" || g.Site != "synthetic-office" || g.Kind != "app" || g.ID != "lab/"+o.runID+"/sim-load-publisher" || g.Principal != "lab/sim-load-publisher" || g.Source != (world.Source{Device: "sim-load-publisher", App: "wendy-world", Service: "sim-load-publisher", Stream: "state"}) || !reflect.DeepEqual(g.Scopes, []string{"publish", "read"}) || len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		clear(cfg.PrivateKey)
		return client.Config{}, nil, errors.New("load profile is not the dedicated simulation publisher")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.RootPublicKey, Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(8)})
	if err == nil {
		err = v.UpdateRevocations(trust.Revocations)
	}
	if err == nil {
		_, err = v.VerifyGrant(cfg.Grant, "publish")
	}
	if err != nil {
		clear(cfg.PrivateKey)
		return client.Config{}, nil, errors.New("load trust or grant invalid")
	}
	c, err := client.New(cfg)
	if err != nil {
		clear(cfg.PrivateKey)
		return client.Config{}, nil, errors.New("load client refused")
	}
	return cfg, c, nil
}

func component(cfg client.Config, run string, epoch, sequence uint64, entity int, now time.Time) world.Component {
	payload, _ := json.Marshal(struct {
		Synthetic   bool   `json:"synthetic"`
		EntityIndex int    `json:"entity_index"`
		Sequence    uint64 `json:"sequence"`
		Value       int    `json:"value"`
	}{true, entity, sequence, int(sequence % 100)})
	return world.Component{Key: world.Key{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Entity: fmt.Sprintf("load/%s/%03d", run, entity), Type: "wendy.load.scalar.v1", Source: cfg.Grant.Grant.Source}, Version: world.Version{Epoch: epoch, Sequence: sequence}, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Timing: world.Timing{SampleTime: now.UTC(), ClockDomain: "simulation:" + run, ErrorKnown: true}, TTL: 30 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "supervision-load-fixture-v1", ModelID: "synthetic-load-scalar", ModelVersion: "1", Simulation: &world.SimulationContext{RunID: run, ScenarioVersion: "office-frame-v1", Seed: 0, ClockDomain: "simulation:" + run}}, Payload: payload}
}

func failureClass(err error) string {
	var status *client.HTTPStatusError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout_or_cancel"
	}
	if errors.As(err, &status) {
		switch status.StatusCode {
		case 401, 403:
			return "authority"
		case 409:
			return "conflict"
		case 429:
			return "rate_limit"
		}
		return "other_http"
	}
	return "transport_or_response"
}

func run(ctx context.Context, o options, output io.Writer) error {
	if o.onsetVariant != "" {
		return runOnset(ctx, o, output)
	}
	if o.preload && o.verifyPreload {
		return errors.New("preload and verification are separate operations")
	}
	if o.verifyPreload {
		return verifyPreload(o, output)
	}
	if o.preload {
		return runPreload(ctx, o, output)
	}
	cfg, c, err := load(o)
	if err != nil {
		return err
	}
	defer clear(cfg.PrivateKey)
	initial, done := context.WithTimeout(ctx, 3*time.Second)
	epoch, err := c.NextEpoch(initial)
	done()
	if err != nil {
		return errors.New("load publisher epoch unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	metrics := runtimeobs.New()
	failures := map[string]uint64{"timeout_or_cancel": 0, "authority": 0, "conflict": 0, "rate_limit": 0, "other_http": 0, "transport_or_response": 0}
	started := time.Now()
	var attempted, published uint64
	interval := time.Second / time.Duration(o.entities*o.hz)
	for ctx.Err() == nil {
		tick := time.Now()
		attempted++
		record := component(cfg, o.runID, epoch, attempted, int((attempted-1)%uint64(o.entities)), tick)
		request, end := context.WithTimeout(ctx, time.Second)
		err := c.Publish(request, record)
		end()
		metrics.Observe(runtimeobs.HTTPOther, time.Since(tick))
		if err == nil {
			published++
		} else {
			metrics.Increment(runtimeobs.HTTPFailures)
			failures[failureClass(err)]++
		}
		if remaining := interval - time.Since(tick); remaining > 0 {
			timer := time.NewTimer(remaining)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	elapsed := time.Since(started)
	return json.NewEncoder(output).Encode(struct {
		Schema            string              `json:"schema"`
		Domain            string              `json:"domain"`
		TargetEntities    int                 `json:"target_entities"`
		TargetHz          int                 `json:"target_hz_per_entity"`
		Attempted         uint64              `json:"attempted"`
		Published         uint64              `json:"published"`
		ElapsedNS         int64               `json:"elapsed_ns"`
		AchievedPerSecond float64             `json:"achieved_publications_per_second"`
		Failures          map[string]uint64   `json:"failures"`
		Metrics           runtimeobs.Snapshot `json:"request_metrics"`
	}{"wendy.world.supervision-load-publisher.v1", "simulation", o.entities, o.hz, attempted, published, elapsed.Nanoseconds(), float64(published) / elapsed.Seconds(), failures, metrics.Snapshot()})
}

func main() {
	var o options
	flag.StringVar(&o.credentials, "credentials", "", "dedicated generated load-publisher profile")
	flag.StringVar(&o.trust, "trust", "", "generated public site trust")
	flag.StringVar(&o.runID, "run-id", "", "exact simulation run")
	flag.DurationVar(&o.duration, "duration", 24*time.Second, "bounded duration, at most40s")
	flag.IntVar(&o.entities, "entities", 200, "live entities, at most200")
	flag.IntVar(&o.hz, "hz", 2, "target publications per entity per second, at most2")
	flag.BoolVar(&o.preload, "preload", false, "publish exactly zero or200 five-minute synthetic records once")
	flag.BoolVar(&o.verifyPreload, "verify-preload", false, "read-only verification of the exact durable preload and origin proofs")
	flag.IntVar(&o.count, "record-count", 200, "preload cardinality, exactly zero or200")
	flag.StringVar(&o.manifest, "manifest", "", "private exact preload digest manifest")
	flag.StringVar(&o.stateDir, "state-dir", "", "explicit read-only node-state mount for verification")
	flag.IntVar(&o.nodeIndex, "node-index", 0, "verification node index1..3")
	flag.IntVar(&o.minFresh, "minimum-fresh-seconds", 60, "verification minimum remaining freshness1..60s")
	flag.StringVar(&o.onsetVariant, "onset-variant", "", "fixed or changing: gated matched retained-record experiment")
	flag.StringVar(&o.release, "release-file", "", "private run-bound publication onset gate")
	flag.StringVar(&o.report, "report-file", "", "new bounded publication gate report")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected load arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
