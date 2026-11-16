package directstream

// FlowSnapshot is captured only after both old ingress and outgoing workers
// have been fenced/joined. OPEN identity/result and generation are validated by
// the session transaction separately, before calling Reconcile.
type FlowSnapshot struct {
	Acked, Offered, Tail, RemoteLimit uint64
	Consumed, Received, Limit         uint64
	SendFinal, ReceiveFinal           uint64
	SendFinished, ReceiveFinished     bool
}

func Snapshot(sender *Sender, receiver *Receiver) FlowSnapshot {
	return FlowSnapshot{Acked: sender.Buffer.Start(), Offered: sender.Offered, Tail: sender.Buffer.End(), RemoteLimit: sender.RemoteLimit,
		Consumed: receiver.Buffer.Start(), Received: receiver.Buffer.End(), Limit: receiver.Buffer.Limit(),
		SendFinal: sender.final, ReceiveFinal: receiver.final, SendFinished: sender.finished, ReceiveFinished: receiver.finished}
}

func (s FlowSnapshot) valid() bool {
	return s.Acked <= s.Offered && s.Offered <= s.Tail && s.Offered <= s.RemoteLimit &&
		s.Consumed <= s.Received && s.Received <= s.Limit &&
		((!s.SendFinished && s.SendFinal == 0) || (s.SendFinished && s.SendFinal == s.Tail)) &&
		((!s.ReceiveFinished && s.ReceiveFinal == 0) || (s.ReceiveFinished && s.ReceiveFinal >= s.Received))
}

func directionConsistent(sender, receiver FlowSnapshot) bool {
	return receiver.Received >= sender.Acked && receiver.Received <= sender.Offered && receiver.Limit >= sender.RemoteLimit &&
		(!receiver.ReceiveFinished || (sender.SendFinished && receiver.ReceiveFinal == sender.SendFinal))
}

// Reconciliation is an immutable plan. Validate every flow before applying any
// plan: failure must not release another flow's replay bytes. On commit, apply
// Outgoing via Sender.State and restart its cursor at Outgoing.Received; replay
// the suffix, including bytes offered but not accepted as a complete frame.
// Incoming FIN is a promise and remains gated on byte consumption, never an
// immediate shutdown of the retained destination socket.
type Reconciliation struct {
	Outgoing         Receipt
	IncomingFinished bool
	IncomingFinal    uint64
	ReplayFIN        bool
}

func Reconcile(local, remote FlowSnapshot) (Reconciliation, error) {
	if !local.valid() || !remote.valid() || !directionConsistent(local, remote) || !directionConsistent(remote, local) {
		return Reconciliation{}, ErrBounds
	}
	return Reconciliation{Outgoing: Receipt{remote.Received, remote.Limit, remote.ReceiveFinished}, IncomingFinished: remote.SendFinished,
		IncomingFinal: remote.SendFinal, ReplayFIN: local.SendFinished && !remote.ReceiveFinished}, nil
}
