package localmesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

type admissionFixture struct {
	mu              sync.Mutex
	capacity        int64
	capacityErr     error
	calls, accepted [][]byte
	outcomes        []error
}

func (f *admissionFixture) CurrentMaxDatagramPayloadSize() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capacity, f.capacityErr
}
func (f *admissionFixture) TrySendDatagram(d []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.calls)
	f.calls = append(f.calls, bytes.Clone(d))
	if index < len(f.outcomes) && f.outcomes[index] != nil {
		return f.outcomes[index]
	}
	f.accepted = append(f.accepted, bytes.Clone(d))
	return nil
}

func wholeIPFrame(id uint32, p []byte) []byte {
	d := make([]byte, fragmentHeader+len(p))
	d[0] = PacketIP
	binary.BigEndian.PutUint32(d[1:5], id)
	binary.BigEndian.PutUint16(d[5:7], uint16(len(p)))
	copy(d[fragmentHeader:], p)
	return d
}

func TestCarrierWholeAndConstrainedFixedForms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capacity   int64
		negotiated int
		count      int
		size       int
	}{
		{"whole", 1356, WholeIPDatagramLimit, 1, TunnelMTU},
		{"path constrained", 1156, WholeIPDatagramLimit, 2, TunnelMTU},
		{"peer constrained", 1356, DatagramLimit, 2, TunnelMTU},
		{"largest baseline", 1156, DatagramLimit, 1, 1091},
		{"first wider packet", 1156, WholeIPDatagramLimit, 1, 1092},
		{"exact current body bound", 1156, WholeIPDatagramLimit, 1, 1147},
		{"above current body bound", 1156, WholeIPDatagramLimit, 2, 1148},
		{"small negotiated constraint", 1156, DatagramLimit, 2, 1092},
		{"exact whole body bound", 1329, WholeIPDatagramLimit, 1, TunnelMTU},
		{"below whole body bound", 1328, WholeIPDatagramLimit, 2, TunnelMTU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &admissionFixture{capacity: tc.capacity}
			sender := &carrierIPSender{sender: f, negotiated: tc.negotiated}
			want, _ := assemblyPacket(t, tc.size, 7)
			if err := sender.sendIP(7, want); err != nil {
				t.Fatal(err)
			}
			if len(f.accepted) != tc.count {
				t.Fatal("wrong actual admitted frame count", len(f.accepted))
			}
			receiver := IPAssembler{wholeIP: true}
			var got []byte
			for i := len(f.accepted) - 1; i >= 0; i-- {
				var err error
				got, err = receiver.Receive(f.accepted[i], time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatal("reordered carrier forms changed IP packet")
			}
			s := sender.snapshot()
			if s.Fragments != uint64(tc.count) || s.ConfiguredPacketSize != 1400 || s.ConfiguredIPDatagram != WholeIPDatagramLimit || s.NegotiatedIPDatagram != tc.negotiated || s.CurrentDatagram != int(tc.capacity) {
				t.Fatalf("incorrect configured/negotiated/current counters %+v", s)
			}
			if tc.count == 1 {
				if len(f.accepted[0]) != len(want)+fragmentHeader || s.WholePackets != 1 {
					t.Fatal("whole form missing")
				}
			} else {
				if len(f.accepted[0]) != 1100 || binary.BigEndian.Uint16(f.accepted[1][7:9]) != 1091 || s.FragmentedPackets != 1 {
					t.Fatal("fallback changed fixed fragment stride")
				}
			}
		})
	}
}

func TestCarrierCapacityShrinkBeforeEnqueueFallsBackOnce(t *testing.T) {
	f := &admissionFixture{capacity: 1356, outcomes: []error{&quic.DatagramTooLargeError{MaxDatagramPayloadSize: 1156}}}
	sender := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	want, _ := assemblyPacket(t, TunnelMTU, 9)
	if err := sender.sendIP(9, want); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || len(f.accepted) != 2 || len(f.calls[0]) != 1329 || len(f.accepted[0]) != 1100 {
		t.Fatal("shrink did not use one pre-enqueue fallback")
	}
	var receiver IPAssembler
	receiver.Receive(f.accepted[1], time.Now())
	got, err := receiver.Receive(f.accepted[0], time.Now())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("fallback ID/form changed", err)
	}
	if s := sender.snapshot(); s.WholeFallbacks != 1 || s.WholePackets != 0 || s.FragmentedPackets != 1 || s.Fragments != 2 || s.Bytes != uint64(len(f.accepted[0])+len(f.accepted[1])) {
		t.Fatalf("rejected attempt counted as wire/enqueue %+v", s)
	}
}

func TestCarrierRefusalDoesNotRestartPartialOrBlockLaterTraffic(t *testing.T) {
	for _, failure := range []error{quic.ErrDatagramQueueFull, &quic.DatagramTooLargeError{MaxDatagramPayloadSize: 200}} {
		f := &admissionFixture{capacity: 1156, outcomes: []error{nil, failure}}
		sender := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
		p, _ := assemblyPacket(t, TunnelMTU, 1)
		if err := sender.sendIP(1, p); err != nil {
			t.Fatal("size/queue refusal killed healthy carrier", err)
		}
		if len(f.calls) != 2 || len(f.accepted) != 1 {
			t.Fatal("partial packet was restarted")
		}
		small, _ := assemblyPacket(t, 80, 2)
		if err := sender.sendIP(2, small); err != nil {
			t.Fatal(err)
		}
		var receiver IPAssembler
		receiver.Receive(f.accepted[0], time.Now())
		got, err := receiver.Receive(f.accepted[1], time.Now())
		if err != nil || !bytes.Equal(got, small) {
			t.Fatal("incomplete prior packet blocked fresh traffic", err)
		}
		if s := sender.snapshot(); s.PartialDrops != 1 || s.Fragments != 2 || s.QueueDrops+s.SizeDrops != 1 {
			t.Fatalf("incorrect partial admission counters %+v", s)
		}
	}
	// Queue pressure on a prospective whole packet is a single loss, not an
	// excuse to enqueue twice as many baseline fragments into the full queue.
	f := &admissionFixture{capacity: 1356, outcomes: []error{quic.ErrDatagramQueueFull}}
	s := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	p, _ := assemblyPacket(t, TunnelMTU, 1)
	if err := s.sendIP(1, p); err != nil || len(f.calls) != 1 || s.snapshot().WholeFallbacks != 0 {
		t.Fatal("queue-full retried/fell back", err)
	}
}

func TestCarrierErrorsAndStatsRemainHonest(t *testing.T) {
	terminal := errors.New("connection closed")
	f := &admissionFixture{capacity: 1156, outcomes: []error{terminal}}
	s := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	p, _ := assemblyPacket(t, 80, 1)
	if err := s.sendIP(1, p); !errors.Is(err, terminal) {
		t.Fatal("terminal transport failure hidden", err)
	}
	if s.snapshot().QueueDrops+s.snapshot().SizeDrops != 0 {
		t.Fatal("terminal failure claimed as deliberate size loss")
	}
	f.capacityErr = terminal
	if err := s.sendIP(2, p); !errors.Is(err, terminal) || len(f.calls) != 1 {
		t.Fatal("invalid capacity initiated send", err)
	}
	if _, err := EncodeIP(3, []byte("invalid")); err == nil {
		t.Fatal("fixture invalid")
	}
	if err := s.sendIP(3, []byte("invalid")); err == nil || len(f.calls) != 1 {
		t.Fatal("invalid IP admitted")
	}
}

func TestWholeFormRequiresCapabilityAndNeverChangesFallbackOffsets(t *testing.T) {
	p, old := assemblyPacket(t, TunnelMTU, 1)
	whole := wholeIPFrame(1, p)
	now := time.Unix(100, 0)
	var constrained IPAssembler
	if _, err := constrained.Receive(whole, now); err == nil || constrained.Snapshot().PendingBytes != 0 {
		t.Fatal("unnegotiated larger frame admitted")
	}
	wide := IPAssembler{wholeIP: true}
	got, err := wide.Receive(whole, now)
	if err != nil || !bytes.Equal(got, p) {
		t.Fatal("negotiated whole form rejected", err)
	}
	if got, err := wide.Receive(whole, now); err != nil || got != nil {
		t.Fatal("whole replay duplicated delivery", err)
	}
	for _, bad := range [][]byte{wholeIPFrame(2, p)[:1200], bytes.Clone(old[1])} {
		if len(bad) == len(old[1]) {
			binary.BigEndian.PutUint16(bad[7:9], 1000)
		}
		if _, err := wide.Receive(bad, now); err == nil {
			t.Fatal("arbitrary stride/partial whole accepted")
		}
	}
	partial := IPAssembler{wholeIP: true}
	partial.Receive(old[0], now)
	if _, err := partial.Receive(whole, now); err == nil || partial.Snapshot().PendingBytes != 0 {
		t.Fatal("mixed layout did not retire conflict")
	}
}

// Keep receive bounds and pending progress independent of the new whole form.
// A rapid stream of whole packets must not evict a still-live slow-BLE-style
// fixed-form packet, or let its eventual completed ID resurrect after churn.
func TestCarrierWholeChurnPreservesExistingPendingAndBounds(t *testing.T) {
	now := time.Unix(1000, 0)
	packet, parts := assemblyPacket(t, TunnelMTU, 1)
	a := IPAssembler{wholeIP: true}
	if _, err := a.Receive(parts[0], now); err != nil {
		t.Fatal(err)
	}
	for id := uint32(2); id < 4096; id++ {
		got, err := a.Receive(wholeIPFrame(id, packet), now.Add(time.Second))
		if err != nil || !bytes.Equal(got, packet) {
			t.Fatal("whole progress lost during churn", id, err)
		}
	}
	if s := a.Snapshot(); s.PendingPackets != 1 || s.PendingBytes != TunnelMTU || s.PeakPendingBytes > ipPendingByteLimit {
		t.Fatalf("whole frames changed incomplete allocation bounds: %+v", s)
	}
	got, err := a.Receive(parts[1], now.Add(8*time.Second))
	if err != nil || !bytes.Equal(got, packet) {
		t.Fatal("serial-window churn invalidated existing pending completion", err)
	}
	if got, err := a.Receive(wholeIPFrame(1, packet), now.Add(9*time.Second)); err != nil || got != nil || a.Snapshot().PendingBytes != 0 {
		t.Fatal("old completed ID reopened after whole churn", err)
	}
}

func TestCarrierSnapshotsConcurrentWithAdmission(t *testing.T) {
	f := &admissionFixture{capacity: 1356}
	sender := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	p, _ := assemblyPacket(t, TunnelMTU, 1)
	done := make(chan struct{})
	var snapshots sync.WaitGroup
	for i := 0; i < 4; i++ {
		snapshots.Add(1)
		go func() {
			defer snapshots.Done()
			for {
				select {
				case <-done:
					return
				default:
					s := sender.snapshot()
					if s.Fragments != s.WholePackets || s.Bytes != s.Fragments*WholeIPDatagramLimit {
						t.Error("torn accepted-work snapshot")
						return
					}
				}
			}
		}()
	}
	for id := uint32(1); id <= 2000; id++ {
		if err := sender.sendIP(id, p); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	snapshots.Wait()
	if s := sender.snapshot(); s.WholePackets != 2000 || s.Bytes != 2000*WholeIPDatagramLimit {
		t.Fatalf("lost accepted-work count: %+v", s)
	}
}

func TestCarrierWorkspaceReusePreservesAdmittedBytes(t *testing.T) {
	f := &admissionFixture{capacity: 1356}
	sender := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	want := make(map[uint32][]byte)
	for id := uint32(1); id <= 60; id++ {
		// Exercise a whole frame immediately after a last fragment with a
		// nonzero offset, as well as short frames after full workspace writes.
		size := TunnelMTU
		f.capacity = 1356
		if id%3 == 1 {
			f.capacity = 1156
		} else if id%3 == 0 {
			size = 80
		}
		packet, _ := assemblyPacket(t, size, id)
		packet[len(packet)-1] = byte(id)
		want[id] = bytes.Clone(packet)
		if err := sender.sendIP(id, packet); err != nil {
			t.Fatal(err)
		}
		// The caller may also recycle its input once admission returns.
		clear(packet)
	}
	verifyCarrierPackets(t, f.accepted, want)
}

func TestCarrierWorkspaceConcurrentSends(t *testing.T) {
	f := &admissionFixture{capacity: 1156}
	sender := &carrierIPSender{sender: f, negotiated: WholeIPDatagramLimit}
	want := make(map[uint32][]byte)
	for id := uint32(1); id <= 200; id++ {
		packet, _ := assemblyPacket(t, TunnelMTU, id)
		packet[len(packet)-1] = byte(id)
		want[id] = packet
	}
	var workers sync.WaitGroup
	for worker := uint32(0); worker < 8; worker++ {
		workers.Go(func() {
			for id := worker + 1; id <= 200; id += 8 {
				if err := sender.sendIP(id, want[id]); err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Wait()
	verifyCarrierPackets(t, f.accepted, want)
	if stats := sender.snapshot(); stats.FragmentedPackets != 200 || stats.Fragments != 400 || stats.PartialDrops != 0 {
		t.Fatalf("concurrent admission accounting: %+v", stats)
	}
}

func verifyCarrierPackets(t *testing.T, frames [][]byte, want map[uint32][]byte) {
	t.Helper()
	// Keep separate receivers so concurrent admission order cannot turn a
	// lower, independently generated test ID into a retired serial number.
	receivers := make(map[uint32]*IPAssembler)
	for _, frame := range frames {
		id := binary.BigEndian.Uint32(frame[1:5])
		if receivers[id] == nil {
			receivers[id] = &IPAssembler{wholeIP: true}
		}
		got, err := receivers[id].Receive(frame, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			if !bytes.Equal(got, want[id]) {
				t.Fatalf("packet %d changed or delivered twice after buffer reuse", id)
			}
			delete(want, id)
		}
	}
	if len(want) > 0 {
		t.Fatalf("missing %d admitted packets", len(want))
	}
}

func FuzzCarrierWholeAssemblyBounds(f *testing.F) {
	packet := make([]byte, TunnelMTU)
	packet[0] = 0x60
	binary.BigEndian.PutUint16(packet[4:6], TunnelMTU-40)
	f.Add(wholeIPFrame(1, packet))
	fixed, _ := EncodeIP(2, packet)
	f.Add(fixed[0])
	f.Add(fixed[1])
	f.Add([]byte{1, 0, 0, 0, 1, 5, 40, 0, 0})
	f.Add([]byte{1, 255, 255, 255, 255, 255, 255, 0, 0, 0x60})
	f.Fuzz(func(t *testing.T, input []byte) {
		a := IPAssembler{wholeIP: true}
		_, _ = a.Receive(input, time.Unix(100, 0))
		if s := a.Snapshot(); s.PendingPackets > ipPendingLimit || s.PendingBytes > ipPendingByteLimit || s.PendingBytes < 0 {
			t.Fatalf("unbounded whole-capable receiver: %+v", s)
		}
	})
}
