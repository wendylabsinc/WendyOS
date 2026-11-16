package directstream

import "net"

// PromotionRequest returns the exact retained OPEN for a successfully opened
// stream. The actor uses it to bind the replacement transport, not to dial a
// second backend. Pending OPENs must finish or be reconciled before handoff.
// The old carrier must already be fenced/joined and Suspend called. A committed
// upper metadata lane may be active; it never dispatches application bytes.
func (m *ReliableFlows) PromotionRequest(id uint32) (OpenRequest, error) {
	if m.closed || (m.alias != 0 && !m.upperControl) || (m.pending.Kind != 0 && !m.upperControl) {
		return OpenRequest{}, ErrBounds
	}
	f, e, err := m.entry(id)
	if err != nil {
		return OpenRequest{}, err
	}
	if f.promotionDone != nil {
		return OpenRequest{}, ErrPromoted
	}
	if f.reset || (f.lane == nil && f.packet == nil) || e.LocalClosed || e.RemoteClosed {
		return OpenRequest{}, ErrRetired
	}
	if !e.Complete {
		return OpenRequest{}, ErrOpenPending
	}
	if e.Result != 0 {
		return OpenRequest{}, ErrRetired
	}
	return f.request, nil
}

// CommitPromotedFlow transfers one existing app socket onto its authenticated
// upper stream. Caller must have committed the session promotion transaction,
// applied BOTH frozen views and completed BindPromotedFlow for this exact ID.
// This method supplies ownership/scheduling, not authentication or negotiation.
//
// A successful first transfer permanently disables BLE rebinding, new direct
// OPENs and byte snapshots for this owner. Remaining flow transfers still work;
// neither a partial failure nor transport loss can send a promoted socket back
// through BLE replay. New app dials belong to the normal upper transport path.
// Session Close closes every stream then joins all prefix workers. Entries are
// retained for session-level bilateral quiescence; local EOF is not proof that
// the peer has released its ownership. On failure raw still belongs to caller.
func (m *ReliableFlows) CommitPromotedFlow(id uint32, raw PromotedStream) (<-chan struct{}, error) {
	if _, err := m.PromotionRequest(id); err != nil {
		return nil, err
	}
	f := m.flows[id]
	if f.lane == nil {
		return nil, ErrBounds
	}
	done, err := f.lane.Conn.CommitPromotion(raw)
	if err != nil {
		return nil, err
	}
	f.promotionDone = done
	m.promoted = true
	return done, nil
}

// PromotionIDs snapshots successful live flow identities before transfer.
// The actor must hold new OPENs and backend completions while negotiating.
// Pending OPENs are omitted until their existing backend/result completes on
// the promoted control lane. They never authorize a duplicate backend dial.
func (m *ReliableFlows) PromotionIDs() ([]uint32, error) {
	if m.closed || (m.promoted && !m.upperControl) {
		return nil, ErrBounds
	}
	var ids []uint32
	for _, id := range m.ids() {
		f, e, err := m.entry(id)
		if err != nil {
			return nil, err
		}
		if f.promotionDone != nil || f.reset || e.LocalClosed || e.RemoteClosed {
			continue
		}
		if !e.Complete {
			continue
		}
		if e.Result == 0 && (f.lane != nil || f.packet != nil) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// BindPromotedControl keeps only OPEN/result/reset/close metadata on the upper
// control connection after the session promotion transaction commits. DATA,
// STATE and FIN are never scheduled or accepted here. New OPENs are forbidden;
// only immutable OPENs already present in the reconciled union may be replayed.
func (m *ReliableFlows) BindPromotedControl(alias uint32) error {
	if m.closed || m.promoted || m.alias != 0 || alias == 0 {
		return ErrBounds
	}
	m.alias = alias
	m.promoted = true
	m.upperControl = true
	m.creditPending = false
	m.datagramLimitPending = false
	return nil
}

// CommitPromotedDatagram binds the existing admitted UDP association. The
// session transaction and per-flow identity proof must already be committed.
// raw preserves datagram boundaries through the upper transport's own framing.
func (m *ReliableFlows) CommitPromotedDatagram(id uint32, raw net.Conn) (<-chan struct{}, error) {
	if _, err := m.PromotionRequest(id); err != nil {
		return nil, err
	}
	f := m.flows[id]
	if f.packet == nil {
		return nil, ErrBounds
	}
	done, err := f.packet.CommitPromotion(raw)
	if err != nil {
		return nil, err
	}
	f.promotionDone = done
	m.promoted = true
	return done, nil
}
