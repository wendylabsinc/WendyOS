package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimediag"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"golang.org/x/sys/unix"
)

func supervisionInput(path string, private bool, out any) error {
	if path == "" {
		return errors.New("explicit supervision input path required")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("supervision input unavailable")
	}
	f := os.NewFile(uintptr(fd), "supervision-input")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || (private && st.Mode().Perm()&0077 != 0) {
		return errors.New("supervision input must be a regular file with appropriate permissions")
	}
	b, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	defer clear(b)
	if err != nil || len(b) > authority.MaxEnvelopeBytes {
		return errors.New("supervision input exceeds bound")
	}
	if authority.DecodeStrict(b, out) != nil {
		return errors.New("invalid supervision input JSON")
	}
	return nil
}
func (inv *invocation) runSupervise(args []string, output io.Writer) error {
	return inv.runSuperviseContext(inv.ctx, args, output)
}
func (inv *invocation) runSuperviseContext(ctx context.Context, args []string, output io.Writer) error {
	f := flag.NewFlagSet("task supervise", flag.ContinueOnError)
	f.SetOutput(inv.stderr)
	credentials := f.String("credentials", "", "private simulation operator credentials")
	trustPath := f.String("trust", "", "explicit public root and signed revocations")
	specPath := f.String("spec", "", "exact immutable supervised task specification")
	runID := f.String("run-id", "", "expected simulation run")
	assignee := f.String("assignee", "", "expected task assignee")
	gateway := f.String("gateway-principal", "", "trusted assignee gateway principal")
	duration := f.Duration("duration", time.Minute, "bounded supervision duration, at most ten minutes")
	diagnostics := f.String("diagnostics-file", "", "optional new local timing snapshot file; no proof material")
	f.Bool("json", true, "bounded JSON line per verified lease")
	if inv.parseFlags(f, args) != nil || f.NArg() != 0 || *runID == "" || *assignee == "" || *gateway == "" || *duration < client.SupervisionRefresh || *duration > 10*time.Minute {
		return errors.New("supervise requires explicit credentials, trust, spec, run, assignee and gateway; duration 100ms to 10m")
	}
	var cfg client.Config
	var trust client.SupervisionTrust
	var spec tasking.Spec
	if err := supervisionInput(*credentials, true, &cfg); err != nil {
		return err
	}
	defer clear(cfg.PrivateKey)
	if err := supervisionInput(*trustPath, false, &trust); err != nil {
		return err
	}
	if err := supervisionInput(*specPath, false, &spec); err != nil {
		return err
	}
	if spec.Assignee != *assignee {
		return errors.New("spec assignee differs from explicit supervision target")
	}
	var metrics *runtimeobs.Collector
	if *diagnostics != "" {
		metrics = runtimeobs.New()
	}
	supervisor, err := client.NewSupervisor(cfg, trust, client.SupervisionTarget{Spec: spec, RunID: *runID, GatewayPrincipal: *gateway, Now: time.Now, Metrics: metrics})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	if metrics == nil {
		return supervisionLoop(ctx, supervisor.Renew, output)
	}
	writer, err := runtimediag.Open(*diagnostics, metrics)
	if err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() {
		err := writer.Run(ctx)
		if err != nil {
			fmt.Fprintln(inv.stderr, "supervision diagnostics export failed; supervision continues")
		}
		finished <- err
	}()
	err = supervisionLoop(ctx, supervisor.Renew, output)
	cancel()
	<-finished // Diagnostic failure never changes the supervision control result.
	return err
}

func supervisionLoop(ctx context.Context, renew func(context.Context) (client.SupervisionLease, error), output io.Writer) error {
	encoder := json.NewEncoder(output)
	var acknowledged uint64
	for {
		lease, err := renew(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return encoder.Encode(map[string]any{"schema": "wendy.world.supervision-ended.v1", "reason": "local_duration_or_cancellation", "acknowledged_leases": acknowledged, "robot_state": "unknown"})
			}
			return err
		}
		if err = encoder.Encode(lease); err != nil {
			return err
		}
		acknowledged++
		// Start the interval after the completed round. A fixed ticker can queue
		// a tick during a slow exchange and immediately violate the gateway's
		// minimum challenge interval. A new timer never catches up missed ticks.
		next := time.NewTimer(client.SupervisionRefresh)
		select {
		case <-ctx.Done():
			next.Stop()
			return encoder.Encode(map[string]any{"schema": "wendy.world.supervision-ended.v1", "reason": "local_duration_or_cancellation", "acknowledged_leases": acknowledged, "robot_state": "unknown"})
		case <-next.C:
		}
	}
}
