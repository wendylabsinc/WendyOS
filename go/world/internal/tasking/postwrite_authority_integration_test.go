package tasking

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type postwriteSignedRig struct {
	*rig
	root        ed25519.PrivateKey
	verifier    *authority.Verifier
	issuer      authority.SignedGrant
	gateway     authority.SignedGrant
	supervisor  authority.SignedGrant
	instruction authority.SignedInstruction
	revocations authority.Revocations
	replay      *postwriteCheckpointReplay
}

type postwriteCheckpointReplay struct {
	authority.ReplayStore
	afterCheckpoint func()
}

func (r *postwriteCheckpointReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	if err := r.ReplayStore.Checkpoint(revision, digest, now); err != nil {
		return err
	}
	if r.afterCheckpoint != nil {
		r.afterCheckpoint()
	}
	return nil
}

// Generated simulation-only credentials exercise the real verifier. No native
// credential lookup, network, production CA, or physical authority is involved.
func setupPostwriteSigned(t *testing.T, mode mission.Mode, shortRevocations bool) *postwriteSignedRig {
	t.Helper()
	base := setup(t)
	if err := base.s.Close(); err != nil {
		t.Fatal(err)
	}
	_, root, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &postwriteSignedRig{rig: base, root: root}
	replay, err := authority.OpenReplay(filepath.Join(t.TempDir(), "authority"), 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replay.Close() })
	r.replay = &postwriteCheckpointReplay{ReplayStore: replay}
	r.verifier, err = authority.NewVerifier(authority.Config{RootPublicKey: root.Public().(ed25519.PublicKey), Tenant: "lab", Site: "office", Domain: "simulation", Audience: "node-robot", Now: func() time.Time { return r.wall }, Replay: r.replay})
	if err != nil {
		t.Fatal(err)
	}
	r.revocations = authority.Revocations{Version: 1, Tenant: "lab", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: r.wall.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: r.wall.Add(30 * time.Minute).UnixMilli()}
	if shortRevocations {
		r.revocations.ExpiresAtUnixMS = r.wall.Add(100 * time.Millisecond).UnixMilli()
	}
	r.installRevocations(t)
	grant := func(principal, kind string, scopes ...string) (authority.SignedGrant, ed25519.PrivateKey) {
		t.Helper()
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		g := authority.Grant{Version: 1, ID: "grant/" + principal, Tenant: "lab", Site: "office", Domain: "simulation", Principal: principal, Kind: kind, Source: world.Source{Device: principal, App: "review", Service: "control", Stream: "state"}, PublicKey: pub, Scopes: scopes, NotBeforeUnixMS: r.wall.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: r.wall.Add(time.Hour).UnixMilli()}
		if kind == "device" {
			g.TaskStatusAudience, g.TaskStatusAssignee = "node-robot", "robot"
		}
		signed, err := authority.SignGrant(g, root)
		if err != nil {
			t.Fatal(err)
		}
		return signed, key
	}
	var issuerKey, gatewayKey, supervisorKey ed25519.PrivateKey
	r.issuer, issuerKey = grant("mission-issuer", "operator", "mission")
	r.gateway, gatewayKey = grant("gateway", "device", "task-status", "supervision-server")
	r.supervisor, supervisorKey = grant("supervisor", "operator", "supervise", "stop")
	r.cfg.MaxLease = 500 * time.Millisecond
	r.cfg.Tenant, r.cfg.Audience, r.cfg.GatewayPrincipal = "lab", "node-robot", "gateway"
	// Same proof and exact-spec contract as node.MissionAuthority; importing
	// node here would create the node -> tasking -> node package cycle.
	r.cfg.ValidateAuthority = func(record Record) error {
		var instruction authority.SignedInstruction
		if err := authority.DecodeStrict(record.Proof, &instruction); err != nil {
			return err
		}
		if _, err := r.verifier.VerifyInstruction(instruction, "mission"); err != nil {
			return err
		}
		var spec Spec
		if err := authority.DecodeStrict(instruction.Payload, &spec); err != nil {
			return err
		}
		a, _ := json.Marshal(spec)
		b, _ := json.Marshal(record.Spec)
		if !bytes.Equal(a, b) {
			return errors.New("stored mission differs from signed proof")
		}
		return nil
	}
	r.cfg.ValidateSupervisor = func(raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
		if _, err := r.verifier.VerifyGrant(r.gateway, "supervision-server"); err != nil {
			return supervision.Ack{}, supervision.Operator{}, err
		}
		return r.verifier.VerifySupervisionAckProof(raw)
	}
	r.s, err = Open(r.path, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.epoch, err = r.s.BeginExecutor("app:nav")
	if err != nil {
		t.Fatal(err)
	}
	spec := r.spec("signed-task")
	spec.Mode = mode
	payload, _ := json.Marshal(spec)
	r.instruction, err = authority.SignInstruction(r.issuer, issuerKey, "node-robot", "mission", payload, r.wall, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(r.instruction)
	id, err := r.verifier.VerifyInstruction(r.instruction, "mission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.s.Accept(spec, Authorization{Principal: id.Principal, Operator: true, AllowAutonomy: true, ValidUntil: id.NotAfter, Proof: proof}); err != nil {
		t.Fatal(err)
	}
	if mode == mission.Supervised {
		operator, err := r.verifier.VerifyGrant(r.supervisor, "supervise")
		if err != nil {
			t.Fatal(err)
		}
		challenge, err := r.s.SupervisionChallenge(spec.ID, SpecDigest(spec), "request-postwrite-0001", supervision.Operator{Principal: operator.Principal, KeySHA256: operator.SigningKeySHA256, GrantID: operator.GrantID, ValidUntilUnixMS: operator.NotAfter.UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := authority.SignSupervisionChallenge(r.gateway, gatewayKey, challenge)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.verifier.VerifySupervisionChallenge(signed, "gateway"); err != nil {
			t.Fatal(err)
		}
		ack, err := authority.SignSupervisionAck(r.supervisor, supervisorKey, supervision.Ack{Challenge: challenge})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(ack)
		receipt, err := r.s.AcknowledgeSupervision(raw)
		if err != nil {
			t.Fatal(err)
		}
		signedReceipt, err := authority.SignSupervisionReceipt(r.gateway, gatewayKey, receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.verifier.VerifySupervisionReceipt(signedReceipt, "gateway"); err != nil || receipt.AckSHA256 != authority.SupervisionAckDigest(ack) {
			t.Fatal("invalid signed receipt", err)
		}
	}
	if d := r.evaluate(t, spec.ID, r.telemetry()); d.Action != mission.Continue || d.Record.Status.Phase != mission.Running {
		t.Fatal("fixture did not reach running", d)
	}
	return r
}

func TestPostwriteSignedAuthorityExpiresDuringFinalCheckpoint(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		t.Run(string(mode), func(t *testing.T) {
			r := setupPostwriteSigned(t, mode, true)
			committed, checkingMission, crossedExpiry := false, false, false
			checks := 0
			verify := r.s.cfg.ValidateAuthority
			r.s.cfg.ValidateAuthority = func(record Record) error {
				checks++
				if committed && !crossedExpiry {
					// Advance after any supervisor checks so the mission's own
					// checkpoint performs a real durable floor update.
					r.wall = r.wall.Add(time.Millisecond)
					r.mono += time.Millisecond
				}
				checkingMission = true
				defer func() { checkingMission = false }()
				return verify(record)
			}
			r.s.beforeCommit = func() {
				if !committed {
					committed = true
				}
			}
			r.replay.afterCheckpoint = func() {
				if committed && checkingMission && !crossedExpiry {
					crossedExpiry = true
					r.wall = r.wall.Add(100 * time.Millisecond)
					r.mono += 100 * time.Millisecond
				}
			}
			d, err := r.s.Evaluate("signed-task", "app:nav", r.epoch, r.telemetry())
			if !committed || !crossedExpiry || checks != 2 {
				t.Fatalf("did not cross expiry in final authority verification: task_commit=%t crossed=%t mission_checks=%d", committed, crossedExpiry, checks)
			}
			if r.wall.UnixMilli() >= r.instruction.ExpiresAtUnixMS || r.wall.UnixMilli() >= r.issuer.Grant.NotAfterUnixMS {
				t.Fatal("fixture accidentally expired instruction or grant")
			}
			if err == nil || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed || d.Supervision != nil {
				t.Fatalf("final checkpoint crossed revocation expiry but returned action=%s phase=%s err=%v supervision_budget=%t", d.Action, d.Record.Status.Phase, err, d.Supervision != nil)
			}
			if _, proofErr := r.verifier.VerifyInstruction(r.instruction, "mission"); !errors.Is(proofErr, authority.ErrStaleRevocations) {
				t.Fatal("verifier does not confirm stale revocations", proofErr)
			}
			before, err := r.s.Get("signed-task")
			if err != nil || before.Status != d.Record.Status {
				t.Fatal("failure not installed", err)
			}
			if err = r.s.Close(); err != nil {
				t.Fatal(err)
			}
			r.s, err = Open(r.path, r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			after, err := r.s.Get("signed-task")
			if err != nil || after.Status != before.Status {
				t.Fatal("failure changed across reopen", err)
			}
		})
	}
}

func (r *postwriteSignedRig) installRevocations(t *testing.T) {
	t.Helper()
	signed, err := authority.SignRevocations(r.revocations, r.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.verifier.UpdateRevocations(signed); err != nil {
		t.Fatal(err)
	}
}

func TestPostwriteSignedMissionAuthority(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		for _, denial := range []string{"principal", "grant", "key", "stale-revocations"} {
			t.Run(string(mode)+"/"+denial, func(t *testing.T) {
				r := setupPostwriteSigned(t, mode, denial == "stale-revocations")
				hooked := false
				r.s.beforeCommit = func() {
					if hooked {
						return
					}
					hooked = true
					if denial == "stale-revocations" {
						r.wall = r.wall.Add(101 * time.Millisecond)
						r.mono += 101 * time.Millisecond
						return
					}
					r.revocations.Revision++
					switch denial {
					case "principal":
						r.revocations.Principals = []string{r.issuer.Grant.Principal}
					case "grant":
						r.revocations.GrantIDs = []string{r.issuer.Grant.ID}
					case "key":
						r.revocations.KeyIDs = []string{authority.KeyID(r.issuer.Grant.PublicKey)}
					}
					r.installRevocations(t)
				}
				d, err := r.s.Evaluate("signed-task", "app:nav", r.epoch, r.telemetry())
				if !hooked {
					t.Fatal("fault was not installed during durable commit")
				}
				_, proofErr := r.verifier.VerifyInstruction(r.instruction, "mission")
				wantErr := authority.ErrDenied
				if denial == "stale-revocations" {
					wantErr = authority.ErrStaleRevocations
				} else {
					for _, check := range []struct {
						grant authority.SignedGrant
						scope string
					}{{r.gateway, "supervision-server"}, {r.supervisor, "supervise"}} {
						if _, e := r.verifier.VerifyGrant(check.grant, check.scope); e != nil {
							t.Fatal("unrelated supervisor or gateway lost authority", e)
						}
					}
				}
				if !errors.Is(proofErr, wantErr) {
					t.Fatal("mission authority was not denied", proofErr)
				}
				if err == nil || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed || d.Supervision != nil {
					t.Fatalf("postwrite invalid mission returned action=%s phase=%s err=%v supervision_budget=%t; real verifier=%v", d.Action, d.Record.Status.Phase, err, d.Supervision != nil, proofErr)
				}
				before, getErr := r.s.Get("signed-task")
				if getErr != nil || before.Status != d.Record.Status {
					t.Fatal("failure was not installed before response", getErr)
				}
				if err = r.s.Close(); err != nil {
					t.Fatal(err)
				}
				r.s, err = Open(r.path, r.cfg)
				if err != nil {
					t.Fatal(err)
				}
				after, err := r.s.Get("signed-task")
				if err != nil || after.Status != before.Status {
					t.Fatal("terminal failure changed across reopen", after.Status, err)
				}
			})
		}
	}
}
