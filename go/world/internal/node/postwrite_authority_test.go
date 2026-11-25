package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// The tasking tests revoke inside its write hook. This endpoint regression uses
// the real persisted phase to revoke only once evaluation has durably entered
// Running, without introducing a new runtime hook or counting verifier calls.
func TestHTTPEvaluateRefusesIssuerRevokedAfterRunningCommit(t *testing.T) {
	f, operator, executor, path, cfg := taskFixture(t)
	if err := f.n.cfg.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	var armed, revoked atomic.Bool
	verify := MissionAuthority(f.v)
	cfg.ValidateAuthority = func(record tasking.Record) error {
		if armed.Load() && !revoked.Load() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var snapshot struct {
				State struct {
					Records map[string]struct {
						Record tasking.Record `json:"record"`
					} `json:"records"`
				} `json:"state"`
			}
			if err := json.Unmarshal(data, &snapshot); err != nil {
				return err
			}
			persisted := snapshot.State.Records[record.Spec.ID].Record
			if persisted.Status.Phase == mission.Running && revoked.CompareAndSwap(false, true) {
				if !bytes.Equal(persisted.Proof, record.Proof) {
					return errors.New("durable mission proof differs from final validation")
				}
				now := time.Now()
				revocation, err := authority.SignRevocations(authority.Revocations{
					Version: authority.Version, Tenant: "tenant", Site: "office", Domain: "simulation",
					Revision: 2, IssuedAtUnixMS: now.Add(-time.Millisecond).UnixMilli(),
					ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(),
					Principals:      []string{operator.Grant.Grant.Principal},
				}, f.root)
				if err != nil {
					return err
				}
				if err := f.v.UpdateRevocations(revocation); err != nil {
					return fmt.Errorf("install issuer revocation: %w", err)
				}
			}
		}
		return verify(record)
	}
	tasks, err := tasking.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tasks.Close() })
	f.n.cfg.Tasks = tasks
	epoch := startExecutor(t, f, executor)
	spec := taskSpec("postwrite-revocation")
	code, body := call(t, f, operator, http.MethodPost, "/v1/tasks", "mission", instructionBody(t, f, operator, "mission", spec))
	if code != http.StatusOK {
		t.Fatalf("admit: %d %s", code, body)
	}
	var accepted tasking.Record
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	code, body = call(t, f, executor, http.MethodPost, "/v1/tasks/evaluate", "execute", jsonBody(t, EvaluateRequest{
		TaskID: spec.ID, ExecutorEpoch: epoch, Telemetry: telemetry(),
	}))
	if !revoked.Load() {
		t.Fatal("endpoint returned without rechecking the durably Running mission")
	}
	if code != http.StatusConflict {
		t.Fatalf("revoked issuer received evaluation success: %d %s", code, body)
	}
	retained, err := tasks.Get(spec.ID)
	if err != nil || retained.Status.Phase != mission.Failed || !bytes.Equal(retained.Proof, accepted.Proof) {
		t.Fatalf("missing retained failure/proof: %+v %v", retained, err)
	}
	if err := verify(retained); !errors.Is(err, authority.ErrDenied) {
		t.Fatalf("real mission verifier did not refuse issuer: %v", err)
	}
	if err := tasks.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := tasking.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.n.cfg.Tasks = reopened
	code, body = call(t, f, executor, http.MethodGet, "/v1/tasks", "read", nil)
	if code != http.StatusOK {
		t.Fatalf("unrevoked executor cannot read retained result: %d %s", code, body)
	}
	var result TasksResponse
	if err := json.Unmarshal(body, &result); err != nil || len(result.Tasks) != 1 || result.Tasks[0].Status != retained.Status || !bytes.Equal(result.Tasks[0].Proof, accepted.Proof) {
		t.Fatalf("reopen changed terminal failure: %s %v", body, err)
	}
}
