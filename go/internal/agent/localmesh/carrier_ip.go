package localmesh

import (
	"errors"
	"sync"

	quic "github.com/quic-go/quic-go"
)

const (
	// WholeIPDatagramLimit fits a complete tunnel packet and its carrier header.
	WholeIPDatagramLimit = TunnelMTU + fragmentHeader
	carrierPacketLimit   = 1400
)

// carrierDatagrams is the nonblocking admission and current-capacity contract.
// Accepted payloads can still be lost after admission; none is a wire receipt.
// TrySendDatagram must own any accepted bytes before returning, allowing its
// caller to reuse the supplied buffer. The pinned quic-go implementation copies
// admitted bytes and returns immediately without retaining rejected buffers.
type carrierDatagrams interface {
	TrySendDatagram([]byte) error
	CurrentMaxDatagramPayloadSize() (int64, error)
}

// CarrierIPStats describes accepted carrier work, not wire delivery. It resets
// with the link. Packet sizes below are byte counts; current capacity is an
// advisory snapshot, not a reservation for the next enqueue.
type CarrierIPStats struct {
	// ConfiguredPacketSize is the complete QUIC UDP-payload ceiling.
	// ConfiguredIPDatagram is the local sender's IP carrier body ceiling.
	// NegotiatedIPDatagram is the peer's authenticated IP carrier body limit.
	// CurrentDatagram is a conservative current QUIC DATAGRAM body limit;
	// consult CurrentDatagramKnown before using it.
	ConfiguredPacketSize, ConfiguredIPDatagram, NegotiatedIPDatagram, CurrentDatagram int
	CurrentDatagramKnown                                                              bool
	// WholePackets counts attempts admitted in one IP carrier datagram,
	// including small packets. FragmentedPackets counts attempts with an
	// admitted first fixed-form piece, even if a later piece was refused.
	// Fragments/Bytes count admitted IP carrier bodies, including headers.
	WholePackets, FragmentedPackets, Fragments, Bytes uint64
	// QueueDrops/SizeDrops count final IP or Babel admission refusals.
	// WholeFallbacks counts size-refused whole attempts that try fixed form.
	// PartialDrops is the subset of final IP refusals after an admitted piece.
	QueueDrops, SizeDrops, WholeFallbacks, PartialDrops uint64
}

type carrierIPSender struct {
	sender     carrierDatagrams
	negotiated int
	mu         sync.Mutex
	stats      CarrierIPStats
	// A single bounded workspace avoids allocating framing bytes which QUIC
	// must immediately copy into its own queue. Keep this lock separate from
	// the stats lock so diagnostics can read while an admission is in progress.
	sendMu sync.Mutex
	frame  [WholeIPDatagramLimit]byte
}

func (s *carrierIPSender) snapshot() CarrierIPStats {
	s.mu.Lock()
	out := s.stats
	s.mu.Unlock()
	out.ConfiguredPacketSize = carrierPacketLimit
	out.ConfiguredIPDatagram = WholeIPDatagramLimit
	out.NegotiatedIPDatagram = s.negotiated
	if size, err := s.sender.CurrentMaxDatagramPayloadSize(); err == nil {
		out.CurrentDatagram = int(size)
		out.CurrentDatagramKnown = true
	}
	return out
}

func (s *carrierIPSender) add(update func(*CarrierIPStats)) {
	s.mu.Lock()
	update(&s.stats)
	s.mu.Unlock()
}

// sendIP preserves the original fixed-position fallback framing. A synchronous
// whole enqueue refusal has sent nothing; once any fallback piece is admitted
// this attempt never restarts or changes its layout.
func (s *carrierIPSender) sendIP(id uint32, packet []byte) error {
	if !validIP(packet) {
		return errors.New("invalid carrier IP packet")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	capacity, err := s.sender.CurrentMaxDatagramPayloadSize()
	if err != nil {
		return err
	}
	if len(packet)+fragmentHeader <= s.negotiated && int64(len(packet)+fragmentHeader) <= capacity && len(packet) > DatagramLimit-fragmentHeader {
		whole := s.frame[:fragmentHeader+len(packet)]
		writeIPFragment(whole, id, packet, 0)
		err = s.sender.TrySendDatagram(whole)
		if err == nil {
			s.add(func(c *CarrierIPStats) { c.WholePackets++; c.Fragments++; c.Bytes += uint64(len(whole)) })
			return nil
		}
		if errors.Is(err, quic.ErrDatagramQueueFull) {
			return s.refusal(err, false)
		}
		var large *quic.DatagramTooLargeError
		if !errors.As(err, &large) {
			return s.refusal(err, false)
		}
		s.add(func(c *CarrierIPStats) { c.WholeFallbacks++ })
	}
	for offset := 0; offset < len(packet); {
		n := min(DatagramLimit-fragmentHeader, len(packet)-offset)
		fragment := s.frame[:fragmentHeader+n]
		writeIPFragment(fragment, id, packet, offset)
		if err = s.sender.TrySendDatagram(fragment); err != nil {
			return s.refusal(err, offset > 0)
		}
		s.add(func(c *CarrierIPStats) {
			c.Fragments++
			c.Bytes += uint64(len(fragment))
			if offset == 0 {
				if n == len(packet) {
					c.WholePackets++
				} else {
					c.FragmentedPackets++
				}
			}
		})
		offset += n
	}
	return nil
}

func (s *carrierIPSender) refusal(err error, partial bool) error {
	if errors.Is(err, quic.ErrDatagramQueueFull) {
		s.add(func(c *CarrierIPStats) {
			c.QueueDrops++
			if partial {
				c.PartialDrops++
			}
		})
		return nil
	}
	var large *quic.DatagramTooLargeError
	if errors.As(err, &large) {
		s.add(func(c *CarrierIPStats) {
			c.SizeDrops++
			if partial {
				c.PartialDrops++
			}
		})
		return nil
	}
	return err
}

func (s *carrierIPSender) sendDatagram(data []byte) error {
	err := s.sender.TrySendDatagram(data)
	if err != nil {
		return s.refusal(err, false)
	}
	return nil
}
