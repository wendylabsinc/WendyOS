package authority

import (
	"encoding/json"
	"testing"
	"time"
)

func operatorInstruction(t *testing.T, f *fixture) SignedInstruction {
	t.Helper()
	g := f.grant.Grant
	g.Kind = "operator"
	g.Scopes = []string{"mission", "stop", "read"}
	var err error
	f.grant, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	i, err := SignInstruction(f.grant, f.key, "node-1", "mission", json.RawMessage(`{"id":"mission-1","mode":"autonomous"}`), f.now, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func TestInstructionImmutableAndDistinctFromDeliveryDeadline(t *testing.T) {
	f := newFixture(t, nil)
	i := operatorInstruction(t, f)
	for _, tamper := range []func(*SignedInstruction){
		func(i *SignedInstruction) { i.Payload = json.RawMessage(`{"id":"mission-2","mode":"autonomous"}`) },
		func(i *SignedInstruction) { i.Audience = "node-2" },
		func(i *SignedInstruction) { i.Operation = "stop" },
		func(i *SignedInstruction) { i.ExpiresAtUnixMS++ },
		func(i *SignedInstruction) { i.Signature[0] ^= 1 },
	} {
		b, _ := json.Marshal(i)
		var changed SignedInstruction
		_ = json.Unmarshal(b, &changed)
		tamper(&changed)
		if _, err := f.verifier.VerifyInstruction(changed, "mission"); err == nil {
			t.Fatal("tampered instruction accepted")
		}
	}
	body, _ := json.Marshal(i)
	request := f.request(t, "mission", "instruction_delivery_123", body)
	if _, err := f.verifier.Verify(request, "mission", body); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.verifier.Verify(request, "mission", body); err == nil {
		t.Fatal("expired delivery accepted")
	}
	if _, err := f.verifier.VerifyInstruction(i, "mission"); err != nil {
		t.Fatal("delivery expiry ended mission", err)
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{f.grant.Grant.ID}
	signed, _ := SignRevocations(rev, f.root)
	if err := f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyInstruction(i, "mission"); err == nil {
		t.Fatal("revoked ongoing instruction accepted")
	}
}
func TestInstructionExpiryAndExpiredRoleStop(t *testing.T) {
	f := newFixture(t, nil)
	i := operatorInstruction(t, f)
	f.now = f.now.Add(30 * time.Minute)
	if _, err := f.verifier.VerifyInstruction(i, "mission"); err == nil {
		t.Fatal("expired instruction accepted")
	}
	g := f.grant.Grant
	g.NotAfterUnixMS = f.now.Add(-time.Second).UnixMilli()
	var err error
	f.grant, err = SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := SignInstruction(f.grant, f.key, "node-1", "stop", json.RawMessage(`{"task_id":"m1"}`), f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyInstruction(stop, "stop"); err != nil {
		t.Fatal("expired role stop denied", err)
	}
}
func TestExecutorGrantBindings(t *testing.T) {
	f := newFixture(t, nil)
	for _, kind := range []string{"agent", "operator", "app", "device"} {
		g := f.grant.Grant
		g.Kind = kind
		g.Scopes = []string{"execute"}
		if _, err := SignGrant(g, f.root); err == nil {
			t.Fatal("missing assignee accepted", kind)
		}
		g.ExecuteAssignee = "robot/navigation"
		signed, err := SignGrant(g, f.root)
		if kind == "agent" || kind == "operator" {
			if err == nil {
				t.Fatal("non-executor role accepted", kind)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		id, err := f.verifier.VerifyGrant(signed, "execute")
		if err != nil || id.ExecuteAssignee != g.ExecuteAssignee {
			t.Fatalf("binding lost: %+v %v", id, err)
		}
	}
}
