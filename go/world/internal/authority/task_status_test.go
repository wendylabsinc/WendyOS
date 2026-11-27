package authority

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func statusGrant(t *testing.T, f *fixture) SignedGrant {
	t.Helper()
	g := f.grant.Grant
	g.ID = "gateway-status"
	g.Principal = "gateway-1"
	g.Kind = "device"
	g.Scopes = []string{"task-status", "read", "replicate"}
	g.TaskStatusAssignee = "robot/navigation"
	g.TaskStatusAudience = "node-1"
	signed, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func statusFixture(t *testing.T, f *fixture) SignedTaskStatus {
	t.Helper()
	s, err := SignTaskStatus(statusGrant(t, f), f.key, "node-1", "robot/navigation", "mission-1", 3, true, json.RawMessage(`{"status":{"Version":3,"Phase":"succeeded"}}`), f.now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTaskStatusGrantRequiresDeviceAndAssigneeOwner(t *testing.T) {
	f := newFixture(t, nil)
	base := statusGrant(t, f).Grant
	for _, kind := range []string{"operator", "agent", "app"} {
		g := base
		g.Kind = kind
		if _, err := SignGrant(g, f.root); err == nil {
			t.Fatalf("%s can attest gateway status", kind)
		}
	}
	for _, mutate := range []func(*Grant){
		func(g *Grant) { g.TaskStatusAudience = "" },
		func(g *Grant) { g.TaskStatusAssignee = "" },
		func(g *Grant) { g.TaskStatusAudience = strings.Repeat("x", 257) },
		func(g *Grant) { g.TaskStatusAssignee = strings.Repeat("x", 257) },
		func(g *Grant) { g.Scopes = []string{"read"} },
	} {
		g := base
		mutate(&g)
		if _, err := SignGrant(g, f.root); err == nil {
			t.Fatal("invalid status grant accepted")
		}
	}
	g := statusGrant(t, f)
	id, err := f.verifier.VerifyGrant(g, "task-status")
	if err != nil || id.TaskStatusAssignee != base.TaskStatusAssignee || id.TaskStatusAudience != base.TaskStatusAudience {
		t.Fatalf("status bindings lost: %+v %v", id, err)
	}
	request, err := SignRequest(g, f.key, "node-2", "task-status", nil, "task_status_request_nonce", f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.verifier.cfg.Audience = "node-2"
	if _, err = f.verifier.Verify(request, "task-status", nil); err == nil {
		t.Fatal("status scope used at nonowning gateway")
	}
}

func TestTaskStatusAuthenticatesOriginAcrossRelayAudiences(t *testing.T) {
	f := newFixture(t, nil)
	s := statusFixture(t, f)
	f.verifier.cfg.Audience = "relay-3"
	id, err := f.verifier.VerifyTaskStatus(s)
	if err != nil || id.Principal != "gateway-1" || id.TaskStatusAudience != "node-1" {
		t.Fatalf("origin status unavailable at relay: %+v %v", id, err)
	}
	for _, mutate := range []func(*SignedTaskStatus){
		func(s *SignedTaskStatus) { s.Audience = "node-2" },
		func(s *SignedTaskStatus) { s.Assignee = "other/navigation" },
		func(s *SignedTaskStatus) { s.TaskID = "mission-2" },
		func(s *SignedTaskStatus) { s.StatusVersion++ },
		func(s *SignedTaskStatus) { s.Terminal = false },
		func(s *SignedTaskStatus) { s.Record = json.RawMessage(`{"status":{"Version":3,"Phase":"failed"}}`) },
		func(s *SignedTaskStatus) { s.SignedAtUnixMS-- },
		func(s *SignedTaskStatus) { s.Grant.Grant.Principal = "other-gateway" },
		func(s *SignedTaskStatus) { s.Signature[0] ^= 1 },
	} {
		data, _ := json.Marshal(s)
		var changed SignedTaskStatus
		if err = json.Unmarshal(data, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if _, err = f.verifier.VerifyTaskStatus(changed); err == nil {
			t.Fatal("tampered status accepted")
		}
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.verifier.cfg.RootPublicKey = other
	if _, err = f.verifier.VerifyTaskStatus(s); err == nil {
		t.Fatal("unknown status grant root accepted")
	}
}

func TestTaskStatusHistoryPreservesOutcomeButAppliesCurrentGatewayRevocation(t *testing.T) {
	f := newFixture(t, nil)
	g := statusGrant(t, f).Grant
	g.NotAfterUnixMS = f.now.Add(time.Minute).UnixMilli()
	grant, err := SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := SignTaskStatus(grant, f.key, "node-1", "robot/navigation", "m1", 1, false, json.RawMessage(`{}`), f.now)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err = f.verifier.VerifyGrant(grant, "task-status"); err == nil {
		t.Fatal("expired grant retained current signing authority")
	}
	if _, err = f.verifier.VerifyTaskStatus(s); err != nil {
		t.Fatal("expiry erased historical status", err)
	}
	if _, err = SignTaskStatus(grant, f.key, "node-1", "robot/navigation", "m1", 2, true, json.RawMessage(`{}`), f.now); err == nil {
		t.Fatal("expired grant signed a new observation")
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.Principals = []string{g.Principal}
	signed, _ := SignRevocations(rev, f.root)
	if err = f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyTaskStatus(s); err == nil {
		t.Fatal("revoked gateway status accepted")
	}
	if _, err = f.verifier.VerifyTaskStatusHistory(s); err != nil {
		t.Fatal("revocation prevented reopening verified history", err)
	}
	s.Record = json.RawMessage(`{"changed":true}`)
	if _, err = f.verifier.VerifyTaskStatusHistory(s); err == nil {
		t.Fatal("historical loader accepted changed gateway status")
	}
}

func TestTaskStatusRejectsFutureTimeWrongOwnerAndOversize(t *testing.T) {
	f := newFixture(t, nil)
	g := statusGrant(t, f)
	for _, args := range []struct{ audience, assignee, taskID string }{
		{"node-2", "robot/navigation", "m1"},
		{"node-1", "other/navigation", "m1"},
		{"node-1", "robot/navigation", ""},
	} {
		if _, err := SignTaskStatus(g, f.key, args.audience, args.assignee, args.taskID, 1, false, json.RawMessage(`{}`), f.now); err == nil {
			t.Fatal("invalid status binding signed")
		}
	}
	for _, data := range []json.RawMessage{nil, json.RawMessage(`x`), json.RawMessage(`"` + strings.Repeat("x", MaxTaskStatusRecordBytes) + `"`)} {
		if _, err := SignTaskStatus(g, f.key, "node-1", "robot/navigation", "m1", 1, false, data, f.now); err == nil {
			t.Fatal("invalid or oversized status payload signed")
		}
	}
	s, err := SignTaskStatus(g, f.key, "node-1", "robot/navigation", "m1", 1, false, json.RawMessage(`{}`), f.now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyTaskStatus(s); err == nil {
		t.Fatal("future status observation accepted")
	}
	s = statusFixture(t, f)
	f.now = time.UnixMilli(f.rev.Revocations.ExpiresAtUnixMS)
	if _, err = f.verifier.VerifyTaskStatus(s); err == nil {
		t.Fatal("stale revocations accepted for retained status")
	}
	if _, err = f.verifier.VerifyTaskStatusHistory(s); err != nil {
		t.Fatal("stale current revocations erased local history", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.verifier.cfg.RootPublicKey = other
	if _, err = f.verifier.VerifyTaskStatusHistory(s); err == nil {
		t.Fatal("historical gateway signature accepted unknown root")
	}
}

func TestTaskStatusRetainsLargeInstructionAndResultRecordWithinBound(t *testing.T) {
	f := newFixture(t, nil)
	g := statusGrant(t, f)
	// A record repeats the signed mission parameters in its original proof and
	// can also retain an inspection result. Its bound exceeds an instruction's.
	for _, size := range []int{65 << 10, MaxTaskStatusRecordBytes} {
		data := json.RawMessage(`"` + strings.Repeat("x", size-2) + `"`)
		s, err := SignTaskStatus(g, f.key, "node-1", "robot/navigation", "large-result", 2, true, data, f.now)
		if err != nil {
			t.Fatal(size, err)
		}
		if _, err = f.verifier.VerifyTaskStatus(s); err != nil {
			t.Fatal(size, err)
		}
	}
	data := json.RawMessage(`"` + strings.Repeat("x", MaxTaskStatusRecordBytes-1) + `"`)
	if _, err := SignTaskStatus(g, f.key, "node-1", "robot/navigation", "too-large", 2, true, data, f.now); err == nil {
		t.Fatal("record exceeding bounded status size accepted")
	}
}

func TestInstructionRelayAndHistoryCannotAuthorizeExpiredWork(t *testing.T) {
	f := newFixture(t, nil)
	i := operatorInstruction(t, f)
	f.verifier.cfg.Audience = "relay-3"
	if _, err := f.verifier.VerifyInstruction(i, "mission"); err == nil {
		t.Fatal("direct verifier ignored own audience")
	}
	if _, err := f.verifier.VerifyInstructionForAudience(i, "mission", "node-1"); err != nil {
		t.Fatal("configured route rejected", err)
	}
	if _, err := f.verifier.VerifyInstructionForAudience(i, "mission", "node-2"); err == nil {
		t.Fatal("wrong configured route accepted")
	}
	f.now = f.now.Add(31 * time.Minute)
	if _, err := f.verifier.VerifyInstructionForAudience(i, "mission", "node-1"); err == nil {
		t.Fatal("expired instruction accepted for relay admission")
	}
	id, err := f.verifier.VerifyInstructionHistoryForAudience(i, "mission", "node-1")
	if err != nil || id.NotAfter.UnixMilli() != i.ExpiresAtUnixMS {
		t.Fatalf("history lost original expiry: %+v %v", id, err)
	}
	rev := f.rev.Revocations
	rev.Revision++
	rev.GrantIDs = []string{i.Grant.Grant.ID}
	signed, _ := SignRevocations(rev, f.root)
	if err = f.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(i, "mission", "node-1"); err != nil {
		t.Fatal("current operator revocation erased history", err)
	}
	if _, err = f.verifier.VerifyInstructionForAudience(i, "mission", "node-1"); err == nil {
		t.Fatal("history verification authorized revoked work")
	}
	// History does not become trusted merely because it is old.
	i.Payload = json.RawMessage(`{"id":"changed"}`)
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(i, "mission", "node-1"); err == nil {
		t.Fatal("tampered historical instruction accepted")
	}
}

func TestInstructionHistoryChecksGrantAtIssuanceAndRoot(t *testing.T) {
	f := newFixture(t, nil)
	i := operatorInstruction(t, f)
	g := i.Grant.Grant
	g.NotAfterUnixMS = f.now.Add(-time.Second).UnixMilli()
	expired, _ := SignGrant(g, f.root)
	bad, err := SignInstruction(expired, f.key, "node-1", "mission", i.Payload, f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(bad, "mission", "node-1"); err == nil {
		t.Fatal("history accepted mission signed after grant expiry")
	}
	stop, err := SignInstruction(expired, f.key, "node-1", "stop", json.RawMessage(`{"task_id":"m1"}`), f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(stop, "stop", "node-1"); err != nil {
		t.Fatal("historical stop lost expired-role exception", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.verifier.cfg.RootPublicKey = other
	if _, err = f.verifier.VerifyInstructionHistoryForAudience(stop, "stop", "node-1"); err == nil {
		t.Fatal("unknown historical root accepted")
	}
}
