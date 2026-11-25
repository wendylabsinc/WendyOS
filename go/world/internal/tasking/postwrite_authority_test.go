package tasking

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

func postwriteReady(t *testing.T, mode mission.Mode, configure func(*supervisionRig, *Spec, *Authorization)) (*supervisionRig, Record) {
	t.Helper()
	r := setupSupervision(t)
	spec, auth := r.spec("postwrite"), r.auth()
	spec.Mode = mode
	if configure != nil {
		configure(r, &spec, &auth)
	}
	if _, err := r.s.Accept(spec, auth); err != nil {
		t.Fatal(err)
	}
	if mode == mission.Supervised {
		r.ack(t, r.challenge(t, spec.ID, "operator-a", "postwrite"))
	}
	d := r.evaluate(t, spec.ID, r.telemetry())
	if d.Action != mission.Continue {
		t.Fatal("fixture did not start", d)
	}
	return r, d.Record
}

func TestPostwriteAuthorityValidatesExactCopiedMissionTwice(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		t.Run(string(mode), func(t *testing.T) {
			r, original := postwriteReady(t, mode, nil)
			calls, writes := 0, 0
			writer := r.s.write
			r.s.write = func(path string, data []byte) error { writes++; return writer(path, data) }
			r.s.cfg.ValidateAuthority = func(record Record) error {
				calls++
				if SpecDigest(record.Spec) != SpecDigest(original.Spec) || !bytes.Equal(record.Proof, original.Proof) {
					t.Fatal("proof/spec changed between checks")
				}
				if (calls == 1 && writes != 0) || (calls == 2 && writes != 1) {
					t.Fatal("authority check in wrong commit order", calls, writes)
				}
				// Mutating the callback copy must not replace retained authority.
				record.Proof[0] = 'x'
				record.Spec.RequiredSensors[0] = "changed"
				return nil
			}
			d, err := r.s.Evaluate(original.Spec.ID, "app:nav", r.epoch, r.telemetry())
			if err != nil || d.Action != mission.Continue || calls != 2 || writes != 1 {
				t.Fatal(d, err, calls, writes)
			}
			if !bytes.Equal(d.Record.Proof, original.Proof) || SpecDigest(d.Record.Spec) != SpecDigest(original.Spec) {
				t.Fatal("callback alias changed retained record")
			}
		})
	}
}

func TestPostwriteAuthorityCheckRunsAfterLateSupervisorValidation(t *testing.T) {
	r, original := postwriteReady(t, mission.Supervised, nil)
	denied := errors.New("issuer revoked during final supervisor validation")
	committed, revoked := false, false
	writer := r.s.write
	r.s.write = func(path string, data []byte) error {
		err := writer(path, data)
		if err == nil {
			committed = true
		}
		return err
	}
	r.verifyHook = func() {
		if committed {
			revoked = true
		}
	}
	calls := 0
	r.s.cfg.ValidateAuthority = func(Record) error {
		calls++
		if revoked {
			return denied
		}
		return nil
	}
	d, err := r.s.Evaluate(original.Spec.ID, "app:nav", r.epoch, r.telemetry())
	if !errors.Is(err, denied) || d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed || d.Supervision != nil || calls != 2 {
		t.Fatal("late supervisor work bypassed mission fence", d, err, calls)
	}
	r.s.Close()
	var openErr error
	r.s, openErr = Open(r.path, r.cfg)
	if openErr != nil {
		t.Fatal(openErr)
	}
	got, openErr := r.s.Get(original.Spec.ID)
	if openErr != nil || got.Status.Phase != mission.Failed {
		t.Fatal("failure lost after reopen", got, openErr)
	}
}

func TestPostwriteAuthorityDelayConsumesOriginalBudgets(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		cases := []string{"telemetry", "mission_deadline", "authority_deadline", "wall_regression", "monotonic_regression"}
		if mode == mission.Supervised {
			cases = append(cases, "lease", "remaining_budget")
		}
		for _, kind := range cases {
			t.Run(string(mode)+"/"+kind, func(t *testing.T) {
				r, original := postwriteReady(t, mode, func(r *supervisionRig, spec *Spec, auth *Authorization) {
					r.s.cfg.MaxTelemetryAge = 250 * time.Millisecond
					if kind == "mission_deadline" {
						spec.NotAfterUnixMS = r.wall.Add(150 * time.Millisecond).UnixMilli()
					}
					if kind == "authority_deadline" {
						auth.ValidUntil = r.wall.Add(150 * time.Millisecond)
					}
					if kind == "lease" {
						r.s.cfg.MaxTelemetryAge = time.Second
					}
				})
				issued := r.wall
				calls := 0
				r.s.cfg.ValidateAuthority = func(Record) error {
					calls++
					if calls == 2 {
						switch kind {
						case "telemetry":
							r.advance(251 * time.Millisecond)
						case "mission_deadline", "authority_deadline":
							r.advance(151 * time.Millisecond)
						case "lease":
							r.advance(501 * time.Millisecond)
						case "remaining_budget":
							r.advance(100 * time.Millisecond)
						case "wall_regression":
							r.wall = r.wall.Add(-time.Millisecond)
						case "monotonic_regression":
							r.mono -= time.Millisecond
						}
					}
					return nil
				}
				d, err := r.s.Evaluate(original.Spec.ID, "app:nav", r.epoch, r.telemetry())
				if calls != 2 {
					t.Fatal("missing final authority check", calls)
				}
				switch kind {
				case "remaining_budget":
					if err != nil || d.Action != mission.Continue || d.Supervision == nil || d.Supervision.RemainingMS != 400 || d.Supervision.ExpiresAtUnixMS != issued.Add(500*time.Millisecond).UnixMilli() {
						t.Fatal("authority delay renewed supervision", d, err)
					}
				case "wall_regression", "monotonic_regression":
					if err == nil || d.Action != mission.Stop || d.Supervision != nil {
						t.Fatal("clock fault allowed authority", d, err)
					}
					if _, err := r.s.Get(original.Spec.ID); err == nil {
						t.Fatal("clock fault was not latched")
					}
				case "mission_deadline", "authority_deadline":
					if d.Action != mission.Stop || d.Record.Status.Phase != mission.Failed || d.Supervision != nil {
						t.Fatal("authority delay bypassed absolute deadline", d, err)
					}
				default:
					if err != nil || d.Action != mission.Hold || d.Record.Status.Phase != mission.Held || d.Supervision != nil {
						t.Fatal("authority delay bypassed freshness/lease", d, err)
					}
					got, err := r.s.Get(original.Spec.ID)
					if err != nil || got.Status.Phase != mission.Held {
						t.Fatal("fallback not durable", got, err)
					}
				}
			})
		}
	}
}

func TestPostwriteAuthorityFailureCommitCannotReturnAuthority(t *testing.T) {
	for _, mode := range []mission.Mode{mission.Autonomous, mission.Supervised} {
		for _, afterReplace := range []bool{false, true} {
			name := "before_replace"
			if afterReplace {
				name = "after_replace"
			}
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				r, original := postwriteReady(t, mode, nil)
				denied, injected := errors.New("current mission authority denied"), errors.New("second task commit failed")
				calls, writes := 0, 0
				r.s.cfg.ValidateAuthority = func(Record) error {
					calls++
					if calls == 2 {
						return denied
					}
					return nil
				}
				writer := r.s.write
				r.s.write = func(path string, data []byte) error {
					writes++
					if writes == 2 {
						if afterReplace {
							if err := writer(path, data); err != nil {
								t.Fatal(err)
							}
						}
						return injected
					}
					return writer(path, data)
				}
				d, err := r.s.Evaluate(original.Spec.ID, "app:nav", r.epoch, r.telemetry())
				if !errors.Is(err, injected) || d.Action != mission.Stop || d.Supervision != nil || d.Record.Status.Version != 0 || writes != 2 {
					t.Fatal("uncertain failure claimed authority or durable status", d, err, writes)
				}
				if _, err := r.s.Get(original.Spec.ID); err == nil {
					t.Fatal("failed commit did not poison store")
				}
				r.s.Close()
				r.s, err = Open(r.path, r.cfg)
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.s.Get(original.Spec.ID)
				if err != nil || got.Status.Phase != mission.Failed || got.Status.Version <= original.Status.Version {
					t.Fatal("interrupted task resumed", got, err)
				}
				if SpecDigest(got.Spec) != SpecDigest(original.Spec) || !bytes.Equal(got.Proof, original.Proof) {
					t.Fatal("recovery replaced immutable mission")
				}
			})
		}
	}
}
