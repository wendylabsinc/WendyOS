package directstream

type RecoveryAction uint8

const (
	RecoveryResume RecoveryAction = iota
	RecoveryReset
	RecoveryReplayOpen     // locally initiated OPEN not yet admitted remotely
	RecoveryReceiveOpen    // peer must replay its original OPEN; no socket dial yet
	RecoveryRetire         // other endpoint already retired a locally closed flow
	RecoveryConfirmRetired // confirm retirement; never recreate this flow
)

type RecoveryStep struct {
	ID           uint32
	Action       RecoveryAction
	OpenComplete bool
	Result       uint16
	Reset        bool
	ResetCode    uint16
	Flow         Reconciliation
}

// PlanRecovery validates the complete union before returning any action. It
// never mutates buffers, registries or sockets. OPEN retries retain their exact
// digest/ID. A missing record at/below high-water is retired, never a fresh OPEN.
// role is the local logical role (0 coordinator/even IDs, 1 participant/odd).
// The session applies this plan only after both proofs and attachment commit.
func PlanRecovery(local, remote RecoveryView, role byte) ([]RecoveryStep, error) {
	return planRecovery(local, remote, role, true)
}

// ValidateRecovery checks the immutable transcript without retaining actions.
// The flow owner builds its mutation plan from current state at commit.
func ValidateRecovery(local, remote RecoveryView, role byte) error {
	_, err := planRecovery(local, remote, role, false)
	return err
}

func planRecovery(local, remote RecoveryView, role byte, retain bool) ([]RecoveryStep, error) {
	if role > 1 || !local.valid() || !remote.valid() {
		return nil, ErrBounds
	}
	for namespace := 0; namespace < 2; namespace++ {
		opener, receiver := local, remote
		if byte(namespace) != role {
			opener, receiver = remote, local
		}
		a, b := opener.Namespaces[namespace], receiver.Namespaces[namespace]
		if a.Admitted < b.Admitted || a.HighWater < b.HighWater || a.Grant > b.Grant || (a.Admitted == b.Admitted) != (a.HighWater == b.HighWater) {
			return nil, ErrBounds
		}
		var unseen uint64
		for _, r := range opener.Records {
			if int(r.ID&1) == namespace && r.ID > b.HighWater {
				if r.OpenComplete || !unopenedBytes(r.Flow) {
					return nil, ErrBounds
				}
				unseen++
			}
		}
		if unseen != a.Admitted-b.Admitted {
			return nil, ErrBounds
		}
	}
	var plan []RecoveryStep
	if retain {
		plan = make([]RecoveryStep, 0, len(local.Records)+len(remote.Records))
	}
	appendStep := func(step RecoveryStep) {
		if retain {
			plan = append(plan, step)
		}
	}
	i, j := 0, 0
	for i < len(local.Records) || j < len(remote.Records) {
		if j == len(remote.Records) || (i < len(local.Records) && local.Records[i].ID < remote.Records[j].ID) {
			r := local.Records[i]
			i++
			action := RecoveryReplayOpen
			if r.ID <= remote.Namespaces[r.ID&1].HighWater {
				if !r.LocalClosed {
					return nil, ErrBounds
				}
				action = RecoveryRetire
			} else if byte(r.ID&1) != role || r.OpenComplete || !unopenedBytes(r.Flow) {
				return nil, ErrBounds
			}
			appendStep(RecoveryStep{ID: r.ID, Action: action, OpenComplete: r.OpenComplete, Result: r.Result, Reset: r.Reset, ResetCode: r.ResetCode})
			continue
		}
		if i == len(local.Records) || remote.Records[j].ID < local.Records[i].ID {
			r := remote.Records[j]
			j++
			action := RecoveryReceiveOpen
			if r.ID <= local.Namespaces[r.ID&1].HighWater {
				if !r.LocalClosed {
					return nil, ErrBounds
				}
				action = RecoveryConfirmRetired
			} else if byte(r.ID&1) == role || r.OpenComplete || !unopenedBytes(r.Flow) {
				return nil, ErrBounds
			}
			appendStep(RecoveryStep{ID: r.ID, Action: action, OpenComplete: r.OpenComplete, Result: r.Result, Reset: r.Reset, ResetCode: r.ResetCode})
			continue
		}
		a, b := local.Records[i], remote.Records[j]
		i++
		j++
		if a.OpenDigest != b.OpenDigest || (a.RemoteClosed && !b.LocalClosed) || (b.RemoteClosed && !a.LocalClosed) {
			return nil, ErrBounds
		}
		opener, receiver := a, b
		if byte(a.ID&1) != role {
			opener, receiver = b, a
		}
		if opener.OpenComplete && !receiver.OpenComplete {
			return nil, ErrBounds
		}
		if opener.OpenComplete && opener.Result != receiver.Result {
			return nil, ErrBounds
		}
		step := RecoveryStep{ID: a.ID, OpenComplete: receiver.OpenComplete, Result: receiver.Result}
		if a.Reset || b.Reset {
			step.Action = RecoveryReset
			step.Reset = true
			step.ResetCode = b.ResetCode
			if a.Reset {
				step.ResetCode = a.ResetCode
			}
		} else {
			if !receiver.OpenComplete || receiver.Result != 0 {
				if !unopenedBytes(a.Flow) || !unopenedBytes(b.Flow) {
					return nil, ErrBounds
				}
			}
			flow, err := Reconcile(a.Flow, b.Flow)
			if err != nil {
				return nil, err
			}
			step.Flow = flow
		}
		appendStep(step)
	}
	return plan, nil
}
func unopenedBytes(s FlowSnapshot) bool {
	return s.Acked == 0 && s.Offered == 0 && s.Tail == 0 && s.Consumed == 0 && s.Received == 0 && !s.SendFinished && !s.ReceiveFinished
}
