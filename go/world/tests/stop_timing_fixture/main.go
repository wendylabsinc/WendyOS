// stop_timing_fixture measures one explicitly scoped simulation stop request.
// It never publishes, renews supervision, issues a mission or retries a request.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
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

const requestDeadline = 2 * time.Second

type options struct{ credentials, trust, spec, runID string }

type report struct {
	Schema               string `json:"schema"`
	Domain               string `json:"domain"`
	RequestAttempted     bool   `json:"request_attempted"`
	ResponseMatchesTask  bool   `json:"response_matches_task"`
	Outcome              string `json:"outcome"`
	ReturnedPhase        string `json:"returned_phase"`
	ErrorClass           string `json:"error_class"`
	HTTPStatus           int    `json:"http_status"`
	InitializationNS     int64  `json:"initialization_ns"`
	InstructionNS        int64  `json:"instruction_ns"`
	RequestNS            int64  `json:"request_ns"`
	ResponseValidationNS int64  `json:"response_validation_ns"`
	TotalNS              int64  `json:"total_ns"`
	RequestDeadlineNS    int64  `json:"request_deadline_ns"`
}

func readPrivate(path string, value any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("stop input unavailable")
	}
	f := os.NewFile(uintptr(fd), "stop-input")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("stop input must be a private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	defer clear(b)
	if err != nil || len(b) > authority.MaxEnvelopeBytes || authority.DecodeStrict(b, value) != nil {
		return errors.New("invalid bounded stop input")
	}
	return nil
}

func load(o options) (cfg client.Config, spec tasking.Spec, c *client.Client, err error) {
	defer func() {
		if err != nil {
			clear(cfg.PrivateKey)
			cfg = client.Config{}
		}
	}()
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(o.runID) {
		return cfg, spec, nil, errors.New("stop requires an explicit simulation run")
	}
	if err = readPrivate(o.credentials, &cfg); err != nil {
		return
	}
	var trust client.SupervisionTrust
	if err = readPrivate(o.trust, &trust); err != nil {
		return
	}
	if err = readPrivate(o.spec, &spec); err != nil {
		return
	}
	g := cfg.Grant.Grant
	u, parseErr := url.Parse(cfg.URL)
	allowedHost := parseErr == nil && u != nil && (u.Host == "world-node-1:8080" || net.ParseIP(u.Hostname()).IsLoopback())
	if !allowedHost || u.Scheme != "http" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		!cfg.AllowInsecureSimulation || cfg.Audience != "node-1" || g.PublishAudience != "" || g.ExecuteAssignee != "" || g.TaskStatusAssignee != "" || g.TaskStatusAudience != "" ||
		g.Domain != "simulation" || g.Tenant != "lab" || g.Site != "synthetic-office" || g.Kind != "operator" || g.ID != "lab/"+o.runID+"/supervisor-a" || g.Principal != "lab/supervisor-a" ||
		g.Source != (world.Source{Device: "supervisor-a", App: "wendy-world", Service: "supervisor-a", Stream: "state"}) || !reflect.DeepEqual(g.Scopes, []string{"supervise", "stop", "read"}) || len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		return cfg, spec, nil, errors.New("stop profile is not the explicit simulation supervisor")
	}
	if spec.ID != "load-mission" || spec.Type != "wendy.nav.go_to.v1" || spec.Site != g.Site || spec.Assignee != "sim-robot/navigation" || spec.Domain != mission.Simulation || spec.Mode != mission.Supervised || spec.Class != mission.Move ||
		spec.Frame != (world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}) || spec.Fallback != mission.Hold || spec.Trigger != nil {
		return cfg, spec, nil, errors.New("stop spec is not the explicit simulation mission")
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: trust.RootPublicKey, Tenant: g.Tenant, Site: g.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(8)})
	if err == nil {
		err = v.UpdateRevocations(trust.Revocations)
	}
	if err == nil {
		_, err = v.VerifyGrant(cfg.Grant, "stop")
	}
	if err != nil {
		return cfg, spec, nil, errors.New("stop trust or grant invalid")
	}
	c, err = client.New(cfg)
	if err != nil {
		err = errors.New("stop client refused")
	}
	return
}

func run(ctx context.Context, o options, output io.Writer) (resultErr error) {
	started := time.Now()
	r := report{Schema: "wendy.world.stop-timing.v1", Domain: "simulation", Outcome: "not_attempted", RequestDeadlineNS: int64(requestDeadline)}
	defer func() {
		r.TotalNS = time.Since(started).Nanoseconds()
		if err := json.NewEncoder(output).Encode(r); err != nil && resultErr == nil {
			resultErr = errors.New("stop report unavailable")
		}
	}()
	cfg, spec, c, err := load(o)
	r.InitializationNS = time.Since(started).Nanoseconds()
	if err != nil {
		r.ErrorClass = "input_refused"
		return errors.New("stop input refused")
	}
	defer clear(cfg.PrivateKey)
	stage := time.Now()
	payload, _ := json.Marshal(struct {
		TaskID string `json:"task_id"`
	}{spec.ID})
	instruction, err := authority.SignInstruction(cfg.Grant, ed25519.PrivateKey(cfg.PrivateKey), cfg.Audience, "stop", payload, stage, time.Minute)
	var body []byte
	if err == nil {
		body, err = json.Marshal(instruction)
	}
	r.InstructionNS = time.Since(stage).Nanoseconds()
	if err != nil {
		r.ErrorClass = "instruction_refused"
		return errors.New("stop instruction refused")
	}
	request, cancel := context.WithTimeout(ctx, requestDeadline)
	defer cancel()
	stage = time.Now()
	r.RequestAttempted, r.Outcome = true, "unknown"
	var raw json.RawMessage
	err = c.Do(request, "stop", "POST", "/v1/tasks/stop", body, &raw)
	r.RequestNS = time.Since(stage).Nanoseconds()
	if err != nil {
		var status *client.HTTPStatusError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			r.ErrorClass = "timeout"
		case errors.Is(err, context.Canceled):
			r.ErrorClass = "cancelled"
		case errors.As(err, &status):
			r.ErrorClass, r.HTTPStatus = "http_refusal", status.StatusCode
		default:
			r.ErrorClass = "transport_or_response"
		}
		return errors.New("stop outcome unconfirmed")
	}
	stage = time.Now()
	var returned tasking.Record
	matched := len(raw) <= authority.MaxEnvelopeBytes && authority.DecodeStrict(raw, &returned) == nil && reflect.DeepEqual(returned.Spec, spec) && returned.Status.Phase.Terminal() && returned.Status.Version > 0
	r.ResponseValidationNS = time.Since(stage).Nanoseconds()
	if !matched {
		r.ErrorClass = "response_mismatch"
		return errors.New("stop response does not match the expected terminal task")
	}
	r.ResponseMatchesTask, r.Outcome, r.ReturnedPhase = true, "terminal_response", string(returned.Status.Phase)
	return nil
}

func main() {
	var o options
	flag.StringVar(&o.credentials, "credentials", "", "generated supervisor-a profile, simulation only")
	flag.StringVar(&o.trust, "trust", "", "generated site root and revocations")
	flag.StringVar(&o.spec, "spec", "", "exact generated supervised load mission")
	flag.StringVar(&o.runID, "run-id", "", "exact generated simulation run")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected stop timing arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
