package directstream

// EnableDatagrams is explicit backend capability, installed before attachment
// or OPEN admission. UDP storage cannot borrow reliable/control reservations.
func (m *ReliableFlows) EnableDatagrams(bytes int, maximum uint16) error {
	if maximum == 0 || int(maximum)*3 > bytes || m.closed || m.alias != 0 || len(m.flows) != 0 || m.datagrams != nil || m.datagramGeneration != 0 {
		return ErrBounds
	}
	b, err := NewDatagramBudget(bytes)
	if err != nil {
		return err
	}
	m.datagrams = b
	m.datagramLimit = maximum
	return nil
}
func (m *ReliableFlows) DatagramsChanged() <-chan struct{} {
	if m.datagrams == nil {
		return nil
	}
	return m.datagrams.Changed()
}
func (m *ReliableFlows) DatagramConnection(id uint32) (*DatagramConn, error) {
	f, e, err := m.entry(id)
	if err != nil {
		return nil, err
	}
	if !e.Complete {
		return nil, ErrOpenPending
	}
	if e.Result != 0 || f.reset || f.packet == nil {
		return nil, ErrRetired
	}
	return f.packet, nil
}
func (m *ReliableFlows) CompleteIncomingDatagram(id uint32, code uint16) (*DatagramConn, error) {
	f, _, err := m.entry(id)
	if err != nil || f.request.Service != ServiceUDP {
		return nil, ErrBounds
	}
	if _, err = m.CompleteIncoming(id, code); err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, nil
	}
	return m.DatagramConnection(id)
}
func (c *DatagramConn) terminalError() error {
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.terminal
}

// PeerDatagramLimit is actor-only. Unknown capacity must defer a new UDP OPEN.
func (m *ReliableFlows) PeerDatagramLimit() (uint16, bool) {
	return m.peerDatagramLimit, m.peerDatagramKnown
}
