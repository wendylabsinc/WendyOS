package tasking

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

func TestLegacyTaskStoreMigratesWithoutResumingMotion(t *testing.T) {
	r := setup(t)
	r.accept(t, "old")
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var old state
	if err := json.Unmarshal(env.State, &old); err != nil {
		t.Fatal(err)
	}
	old.Version = 1
	env.State, _ = json.Marshal(old)
	hash := sha256.Sum256(env.State)
	env.SHA256 = hex.EncodeToString(hash[:])
	data, _ = json.Marshal(env)
	if err := os.WriteFile(r.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r.mono = 0
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.s.Get("old")
	if err != nil || got.Status.Phase != mission.Failed {
		t.Fatal("legacy motion resumed", got, err)
	}
	data, err = os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.State, &old); err != nil {
		t.Fatal(err)
	}
	if old.Version != taskStoreVersion || len(old.AuditID) != 64 {
		t.Fatal("old binary could silently discard refusal or audit identity state", old.Version)
	}
}

func TestRefusalsCannotBeRelabeledAsLegacyState(t *testing.T) {
	r := setup(t)
	if _, err := r.s.Reject(r.spec("no-reuse"), r.auth(), "unsupported"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var saved state
	if err := json.Unmarshal(env.State, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Version = 1
	env.State, _ = json.Marshal(saved)
	hash := sha256.Sum256(env.State)
	env.SHA256 = hex.EncodeToString(hash[:])
	data, _ = json.Marshal(env)
	if err := os.WriteFile(r.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if restored, err := Open(r.path, r.cfg); err == nil {
		restored.Close()
		t.Fatal("version one state accepted new durable refusal fields")
	}
}

func TestRejectedTaskNeverExecutesOrChangesAfterRestart(t *testing.T) {
	r := setup(t)
	spec := r.spec("unsupported")
	spec.Type = "unknown.adapter.v1"
	refused, err := r.s.Reject(spec, r.auth(), "unsupported adapter")
	if err != nil || refused.Status.Phase != mission.Rejected || !refused.Status.Phase.Terminal() {
		t.Fatal(refused, err)
	}
	if _, ok := r.s.engine.Checkpoint().Records[spec.ID]; ok {
		t.Fatal("rejected instruction entered execution engine")
	}
	if _, err := r.s.Evaluate(spec.ID, "app:nav", r.epoch, r.telemetry()); err == nil {
		t.Fatal("rejected instruction executed")
	}
	if got, err := r.s.Accept(spec, r.auth()); err != nil || got.Status != refused.Status {
		t.Fatal("same instruction renewed rejection", got, err)
	}
	changed := spec
	changed.Type = "go_to"
	if _, err := r.s.Accept(changed, r.auth()); err == nil {
		t.Fatal("changed spec reused rejected ID")
	}
	if got, err := r.s.Stop(spec.ID, r.auth()); err != nil || got.Status != refused.Status {
		t.Fatal("stop changed terminal rejection", got, err)
	}
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	r.mono = 0
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.BeginExecutor("app:nav"); err != nil {
		t.Fatal(err)
	}
	got, err := r.s.Get(spec.ID)
	if err != nil || got.Status != refused.Status || string(got.Proof) != string(refused.Proof) {
		t.Fatal("reopen changed refusal", got, err)
	}
	all, err := r.s.List()
	if err != nil || len(all) != 1 || all[0].Status != refused.Status {
		t.Fatal(all, err)
	}
	audit, err := r.s.AuditLog()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range audit {
		if event.TaskID != spec.ID {
			continue
		}
		count++
		if event.Event != "rejected" {
			t.Fatal("rejection acquired execution event", event)
		}
	}
	if count != 1 {
		t.Fatal("rejection audit duplicated or absent", audit)
	}
}

func TestRefusalPreservesExistingAssigneeDecision(t *testing.T) {
	r := setup(t)
	original := r.accept(t, "same-id")
	changed := original.Spec
	changed.Type = "unsupported"
	got, err := r.s.Reject(changed, r.auth(), "new conflicting instruction")
	if err != nil || got.Spec.Type != original.Spec.Type || got.Status != original.Status {
		t.Fatal("refusal replaced an admitted decision", got, err)
	}
}

func TestRejectedAuthorityQuotaAndFailedPersistence(t *testing.T) {
	t.Run("authority", func(t *testing.T) {
		r := setup(t)
		r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
		if _, err := r.s.Reject(r.spec("x"), r.auth(), "unsupported"); err == nil {
			t.Fatal("revoked rejection committed")
		}
		all, _ := r.s.List()
		if len(all) != 0 {
			t.Fatal(all)
		}
	})
	t.Run("quota", func(t *testing.T) {
		r := setup(t)
		r.s.cfg.MaxTasks = 1
		if _, err := r.s.Reject(r.spec("x"), r.auth(), "unsupported"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Accept(r.spec("y"), r.auth()); err == nil {
			t.Fatal("rejected records escaped quota")
		}
		if _, err := r.s.Stop("y", r.auth()); err == nil {
			t.Fatal("stop escaped quota")
		}
	})
	t.Run("write failure", func(t *testing.T) {
		r := setup(t)
		r.s.write = func(string, []byte) error { return errors.New("disk fault") }
		if _, err := r.s.Reject(r.spec("x"), r.auth(), "unsupported"); err == nil {
			t.Fatal("uncommitted rejection acknowledged")
		}
		if err := r.s.Close(); err != nil {
			t.Fatal(err)
		}
		r.mono = 0
		var err error
		r.s, err = Open(r.path, r.cfg)
		if err != nil {
			t.Fatal(err)
		}
		all, err := r.s.List()
		if err != nil || len(all) != 0 {
			t.Fatal("failed commit became visible", all, err)
		}
	})
}
