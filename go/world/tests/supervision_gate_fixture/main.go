// supervision_gate_fixture releases only the fixed simulation supervisor CLI.
// It makes no request, issues no mission and never retries a supervisor.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"golang.org/x/sys/unix"
)

type options struct{ credentials, trust, spec, runID, operator, release, report, diagnostics, observations string }
type release struct {
	Schema          string `json:"schema"`
	RunID           string `json:"run_id"`
	ReleaseUnixMS   int64  `json:"release_unix_ms"`
	WindowEndUnixMS int64  `json:"window_end_unix_ms"`
}
type report struct {
	Schema        string `json:"schema"`
	Domain        string `json:"domain"`
	RunID         string `json:"run_id"`
	Operator      string `json:"operator"`
	Stage         string `json:"stage"`
	ReadyAtUnixMS int64  `json:"ready_at_unix_ms"`
	ExecAtUnixMS  *int64 `json:"exec_at_unix_ms"`
	ReleaseUnixMS *int64 `json:"release_unix_ms"`
	ValidationNS  int64  `json:"validation_ns"`
	WaitNS        int64  `json:"wait_ns"`
	ErrorClass    string `json:"error_class,omitempty"`
}

func readPrivate(path string, out any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "gate-input")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("input refused")
	}
	b, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	defer clear(b)
	if err != nil || len(b) > authority.MaxEnvelopeBytes || authority.DecodeStrict(b, out) != nil {
		return errors.New("input refused")
	}
	return nil
}

func validate(o options) error {
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(o.runID) || (o.operator != "a" && o.operator != "b") {
		return errors.New("gate scope refused")
	}
	paths := map[string]bool{}
	allPaths := []string{o.credentials, o.trust, o.spec, o.release, o.report, o.diagnostics}
	if o.observations != "" {
		allPaths = append(allPaths, o.observations)
	}
	for _, p := range allPaths {
		if !filepath.IsAbs(p) || paths[filepath.Clean(p)] {
			return errors.New("gate paths refused")
		}
		paths[filepath.Clean(p)] = true
	}
	var cfg client.Config
	if err := readPrivate(o.credentials, &cfg); err != nil {
		return errors.New("gate credentials refused")
	}
	defer clear(cfg.PrivateKey)
	var trust client.SupervisionTrust
	var spec tasking.Spec
	if readPrivate(o.trust, &trust) != nil || readPrivate(o.spec, &spec) != nil {
		return errors.New("gate trust or spec refused")
	}
	name := "supervisor-" + o.operator
	endpoint := "http://world-node-1:8080"
	if o.operator == "a" {
		endpoint = "http://supervision-proxy:8099"
	}
	g := cfg.Grant.Grant
	if cfg.URL != endpoint || !cfg.AllowInsecureSimulation || cfg.Audience != "node-1" || g.ID != "lab/"+o.runID+"/"+name || g.Principal != "lab/"+name || g.Tenant != "lab" || g.Site != "synthetic-office" || g.Domain != "simulation" || g.Kind != "operator" || g.Source != (world.Source{Device: name, App: "wendy-world", Service: name, Stream: "state"}) || !reflect.DeepEqual(g.Scopes, []string{"supervise", "stop", "read"}) || g.PublishAudience != "" || g.ExecuteAssignee != "" || g.TaskStatusAssignee != "" || g.TaskStatusAudience != "" || len(cfg.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(cfg.PrivateKey[32:], g.PublicKey) {
		return errors.New("gate profile refused")
	}
	if spec.ID != "load-mission" || spec.Type != "wendy.nav.go_to.v1" || spec.Site != g.Site || spec.Assignee != "sim-robot/navigation" || spec.Domain != mission.Simulation || spec.Mode != mission.Supervised || spec.Class != mission.Move || spec.Frame != (world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}) || spec.Fallback != mission.Hold || spec.Trigger != nil {
		return errors.New("gate mission refused")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.RootPublicKey, Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(8)})
	if err == nil {
		err = v.UpdateRevocations(trust.Revocations)
	}
	if err == nil {
		_, err = v.VerifyGrant(cfg.Grant, "supervise")
	}
	if err != nil {
		return errors.New("gate authority refused")
	}
	return nil
}

func writeReport(path string, value report, first bool) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gate-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if first {
		return os.Link(f.Name(), path)
	} // Refuse an existing output, including symlinks.
	return os.Rename(f.Name(), path)
}

func command(o options) []string {
	return []string{"/world", "task", "supervise", "--credentials", o.credentials, "--trust", o.trust, "--spec", o.spec, "--run-id", o.runID, "--assignee", "sim-robot/navigation", "--gateway-principal", "lab/node-1", "--duration", "40s", "--json", "--diagnostics-file", o.diagnostics}
}

func run(ctx context.Context, o options, exec func(string, []string, []string) error) (resultErr error) {
	return runWithReporter(ctx, o, exec, writeReport)
}

func runWithReporter(ctx context.Context, o options, exec func(string, []string, []string) error, write func(string, report, bool) error) (resultErr error) {
	started := time.Now()
	if err := validate(o); err != nil {
		return err
	}
	r := report{Schema: "wendy.world.supervision-gate.v1", Domain: "simulation", RunID: o.runID, Operator: o.operator, Stage: "ready", ReadyAtUnixMS: time.Now().UnixMilli(), ValidationNS: time.Since(started).Nanoseconds()}
	if err := write(o.report, r, true); err != nil {
		return errors.New("gate report refused")
	}
	waitStarted := time.Now()
	defer func() {
		if resultErr != nil {
			r.Stage = "failed"
			if r.ErrorClass == "" {
				r.ErrorClass = "release_refused"
			}
			r.WaitNS = time.Since(waitStarted).Nanoseconds()
			_ = write(o.report, r, false)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var released release
	for {
		if ctx.Err() != nil {
			r.ErrorClass = "cancelled_or_timeout"
			return errors.New("gate cancelled")
		}
		err := readPrivate(o.release, &released)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("gate release refused")
		}
		select {
		case <-ctx.Done():
			r.ErrorClass = "cancelled_or_timeout"
			return errors.New("gate cancelled")
		case <-ticker.C:
		}
	}
	now := time.Now()
	target := time.UnixMilli(released.ReleaseUnixMS)
	if released.Schema != "wendy.world.supervision-gate-release.v1" || released.RunID != o.runID || released.ReleaseUnixMS <= 0 || released.WindowEndUnixMS-released.ReleaseUnixMS != 24000 || target.Before(now.Add(-2*time.Second)) || target.After(now.Add(2*time.Second)) {
		return errors.New("gate release scope or timing refused")
	}
	r.ReleaseUnixMS = &released.ReleaseUnixMS
	if delay := target.Sub(now); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			r.ErrorClass = "cancelled_or_timeout"
			return errors.New("gate cancelled")
		case <-timer.C:
		}
	}
	if ctx.Err() != nil || time.Now().Before(target) {
		r.ErrorClass = "cancelled_or_clock_regressed"
		return errors.New("gate timing refused")
	}
	n := time.Now().UnixMilli()
	r.ExecAtUnixMS = &n
	r.Stage = "exec"
	r.WaitNS = time.Since(waitStarted).Nanoseconds()
	if err := write(o.report, r, false); err != nil {
		return errors.New("gate report failed")
	}
	// Report I/O can outlast cancellation or the observation window. The child
	// does not inherit this context, so check again before replacing the gate.
	if ctx.Err() != nil || time.Now().UnixMilli() >= released.WindowEndUnixMS || time.Now().Before(target) {
		r.ErrorClass = "cancelled_or_window_elapsed"
		return errors.New("gate release elapsed")
	}
	// Exec replaces the gate; the timestamp records the attempt, not its success.
	if err := exec("/world", command(o), os.Environ()); err != nil {
		r.ErrorClass = "exec_failed"
		if o.observations != "" {
			r.ErrorClass = "supervisor_or_observer_failed"
		}
		return errors.New("gate exec failed")
	}
	return nil
}

func main() {
	var o options
	flag.StringVar(&o.credentials, "credentials", "", "simulation supervisor profile")
	flag.StringVar(&o.trust, "trust", "", "simulation trust")
	flag.StringVar(&o.spec, "spec", "", "exact load mission")
	flag.StringVar(&o.runID, "run-id", "", "simulation run")
	flag.StringVar(&o.operator, "operator", "", "a or b")
	flag.StringVar(&o.release, "release-file", "", "private release file")
	flag.StringVar(&o.report, "report-file", "", "new bounded report")
	flag.StringVar(&o.diagnostics, "diagnostics-file", "", "new supervisor diagnostics")
	flag.StringVar(&o.observations, "observations-file", "", "optional bounded sanitized receipt completion observations")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected gate arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	execute := syscall.Exec
	if o.observations != "" {
		execute = func(_ string, _ []string, _ []string) error { return captureSupervisor(ctx, o) }
	}
	if err := run(ctx, o, execute); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
