package services

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// The same signed entry and device identity cross a real process exit. Only
// the Cloud checkpoint and container action are fixtures; postbox verification,
// atomic state writes, restart loading and signed acknowledgements are real.
type postboxCrashFixture struct {
	Tenant, UUID, URI, Cert, Key, Chain string
	Now                                 time.Time
	TSACA, Entry                        []byte
}

type durablePostboxControl struct{ path string }

func (c durablePostboxControl) AppOf(_ context.Context, name string) string { return name }
func (c durablePostboxControl) StartApp(ctx context.Context, name string) error {
	return c.StopApp(ctx, name)
}
func (c durablePostboxControl) StopApp(_ context.Context, name string) error {
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(name + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

func TestPostboxProcessCrash(t *testing.T) {
	if path := os.Getenv("WENDY_POSTBOX_CRASH_FIXTURE"); path != "" {
		runPostboxCrashChild(t, path, os.Getenv("WENDY_POSTBOX_CRASH_PHASE"))
		return
	}
	for _, phase := range []string{"before_claim", "after_claim", "after_apply", "after_decision", "after_ack"} {
		t.Run(phase, func(t *testing.T) {
			f := newPBFixture(t)
			entry := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
			raw, err := proto.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			fixture := postboxCrashFixture{f.tenant, f.uuid, f.uri, f.certPEM, f.keyPEM, f.chainPEM, f.now, f.tsaCA.Raw, raw}
			data, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.dir, "fixture.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			run := func(mode string, want int) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPostboxProcessCrash$", "-test.v", "-test.count=1")
				cmd.Env = append(os.Environ(), "WENDY_POSTBOX_CRASH_FIXTURE="+path, "WENDY_POSTBOX_CRASH_PHASE="+mode)
				out, err := cmd.CombinedOutput()
				code := 0
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatalf("child: %v: %s", err, out)
					}
					code = exit.ExitCode()
				}
				if ctx.Err() != nil || code != want {
					t.Fatalf("%s exit=%d want=%d context=%v: %s", mode, code, want, ctx.Err(), out)
				}
			}
			run(phase, 73)
			// A new OS process must settle or retry from durable state, not memory.
			run("restart:"+phase, 0)
			// Restart again to prove settlement and ack persistence are themselves durable.
			run("settled:"+phase, 0)
		})
	}
}

func runPostboxCrashChild(t *testing.T, path, phase string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input postboxCrashFixture
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	tsa, err := x509.ParseCertificate(input.TSACA)
	if err != nil {
		t.Fatal(err)
	}
	entry := new(cloudpbv2.PostboxEntry)
	if err := proto.Unmarshal(input.Entry, entry); err != nil {
		t.Fatal(err)
	}
	f := &pbFixture{t: t, tenant: input.Tenant, uuid: input.UUID, uri: input.URI, certPEM: input.Cert, keyPEM: input.Key, chainPEM: input.Chain, now: input.Now, dir: filepath.Dir(path)}
	f.cloud = &fakePostboxCloud{tenant: f.tenant, target: f.uri, entries: []*cloudpbv2.PostboxEntry{entry}, pulls: make(chan int64, 16), inter: []*x509.Certificate{tsa}}
	f.ctr = &mockContainerdClient{}
	f.p = f.newPostbox()
	effects := filepath.Join(f.dir, "effects.txt")
	f.p.control = durablePostboxControl{effects}
	f.p.write = func(dir, name string, data []byte) error {
		var next postboxState
		if err := json.Unmarshal(data, &next); err != nil {
			return err
		}
		applied, _ := os.ReadFile(effects)
		crashBefore := (phase == "before_claim" && next.Pending != nil) ||
			(phase == "after_apply" && len(applied) > 0 && next.Pending == nil && len(next.Outcomes) > 0) ||
			(phase == "after_ack" && next.Acked == 1)
		if crashBefore {
			os.Exit(73)
		}
		if err := writeProvisioningFile(dir, name, data); err != nil {
			return err
		}
		if (phase == "after_claim" && next.Pending != nil) ||
			(phase == "after_decision" && len(applied) > 0 && next.Pending == nil && len(next.Outcomes) > 0) {
			os.Exit(73)
		}
		return nil
	}
	f.cycle()
	if len(phase) < 8 || (phase[:8] != "restart:" && phase[:8] != "settled:") {
		t.Fatal("requested crash boundary was not reached")
	}
	point := phase[8:]
	wantEffects := "web\n"
	wantOutcome := cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED
	if point == "after_claim" {
		wantEffects = ""
	}
	if point == "after_claim" || point == "after_apply" {
		wantOutcome = cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED
	}
	applied, err := os.ReadFile(effects)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if string(applied) != wantEffects {
		t.Fatalf("side effects=%q want=%q", applied, wantEffects)
	}
	st := f.p.snapshot()
	if st.HighWater != 1 || st.Acked != 1 || st.Pending != nil {
		t.Fatalf("restarted state=%+v", st)
	}
	outcome := f.outcome("web")
	if outcome.EntryID != 1 || outcome.CorrelationID == "" || outcome.Outcome != wantOutcome {
		t.Fatalf("outcome=%+v want=%v", outcome, wantOutcome)
	}
	wantAcks := 1
	if phase[:8] == "settled:" {
		wantAcks = 0
	}
	if len(f.cloud.acks) != wantAcks {
		t.Fatalf("acks=%d want=%d", len(f.cloud.acks), wantAcks)
	}
	for _, ack := range f.cloud.acks {
		assertAckShape(t, f, ack, entry)
	}
}
