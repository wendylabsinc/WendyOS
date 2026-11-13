package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func assemblyPacket(t testing.TB, size int, id uint32) ([]byte, [][]byte) {
	t.Helper()
	p := make([]byte, size)
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(size-40))
	for i := 40; i < len(p); i++ {
		p[i] = byte(i) + byte(id)
	}
	ds, err := EncodeIP(id, p)
	if err != nil {
		t.Fatal(err)
	}
	return p, ds
}

func checkAssemblyBounds(t testing.TB, a *IPAssembler) {
	t.Helper()
	s := a.Snapshot()
	total := 0
	for _, p := range a.pending {
		total += len(p.data)
	}
	if s.PendingBytes != total || s.PendingBytes != a.bytes || s.PendingPackets != len(a.pending) || s.PendingPackets > ipPendingLimit || s.PendingBytes > ipPendingByteLimit || s.PendingBytes < 0 || s.PeakPendingBytes > ipPendingByteLimit {
		t.Fatalf("invalid accounting %+v actual bytes=%d packets=%d", s, total, len(a.pending))
	}
}

func TestIPAssemblyFullTableMakesFreshProgress(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	for id := uint32(1); id <= ipPendingLimit; id++ {
		_, d := assemblyPacket(t, TunnelMTU, id)
		if p, err := a.Receive(d[0], now); err != nil || p != nil {
			t.Fatal(err)
		}
	}
	// Equal timestamps must still evict exactly the first admitted incomplete ID.
	want, d := assemblyPacket(t, TunnelMTU, 100)
	a.Receive(d[0], now)
	if _, ok := a.pending[1]; ok {
		t.Fatal("oldest admission survived capacity eviction")
	}
	for id := uint32(2); id <= ipPendingLimit; id++ {
		if _, ok := a.pending[id]; !ok {
			t.Fatalf("unrelated ID%d evicted", id)
		}
	}
	got, err := a.Receive(d[1], now)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("fresh packet stalled behind incomplete loss", err)
	}
	s := a.Snapshot()
	if s.EvictedPackets != 1 || s.ReassembledPackets != 1 || s.PendingPackets != 31 || s.PeakPendingBytes != ipPendingByteLimit {
		t.Fatalf("unexpected counters %+v", s)
	}
	checkAssemblyBounds(t, &a)
	// A late half of the evicted ID cannot steal a new slot.
	_, old := assemblyPacket(t, TunnelMTU, 1)
	a.Receive(old[1], now)
	if s := a.Snapshot(); s.RetiredFragments != 1 || s.PendingPackets != 31 {
		t.Fatalf("evicted ID resurrected %+v", s)
	}
	// Existing pending completion has priority over any new admission/eviction.
	_, kept := assemblyPacket(t, TunnelMTU, 2)
	if got, err := a.Receive(kept[1], now); err != nil || got == nil || a.Snapshot().EvictedPackets != 1 {
		t.Fatal("pending completion caused eviction", err)
	}
}

func TestIPAssemblyRetirementChurnWrapAndPendingAge(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	pending, slow := assemblyPacket(t, TunnelMTU, math.MaxUint32-2)
	a.Receive(slow[1], now)
	// Small traffic may advance beyond the serial window. It must not revoke a
	// young retained partial, including across the uint32 sequence wrap.
	var first []byte
	for i := uint32(0); i < 2*ipRetirementWindow; i++ {
		id := uint32(math.MaxUint32-1) + i
		_, d := assemblyPacket(t, 80, id)
		if i == 0 {
			first = d[0]
		}
		if got, err := a.Receive(d[0], now.Add(time.Second)); err != nil || got == nil {
			t.Fatalf("whole ID%d: %v", id, err)
		}
	}
	got, err := a.Receive(slow[0], now.Add(8*time.Second))
	if err != nil || !bytes.Equal(got, pending) {
		t.Fatal("retained slow BLE partial lost across window/wrap", err)
	}
	before := a.Snapshot()
	for i := 0; i < 10000; i++ {
		a.Receive(slow[i%2], now.Add(9*time.Second))
		a.Receive(first, now.Add(9*time.Second))
	}
	s := a.Snapshot()
	if s.PendingBytes != 0 || s.ReassembledPackets != before.ReassembledPackets || s.WholePackets != before.WholePackets || s.RetiredFragments-before.RetiredFragments != 20000 {
		t.Fatalf("late replay recreated state/delivery %+v", s)
	}
	// A recent completed whole packet is suppressed within the fixed window too.
	_, recent := assemblyPacket(t, 80, 2045)
	a.Receive(recent[0], now.Add(9*time.Second))
	if a.Snapshot().WholePackets != before.WholePackets {
		t.Fatal("completed whole ID delivered twice")
	}
	checkAssemblyBounds(t, &a)
}

func TestIPAssemblyExpiryIsAbsoluteAndSweepsWholeTraffic(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	_, d := assemblyPacket(t, TunnelMTU, 1)
	a.Receive(d[0], now)
	a.Receive(d[0], now.Add(9*time.Second))
	if a.Snapshot().Duplicates != 1 {
		t.Fatal("identical pending duplicate not counted")
	}
	_, whole := assemblyPacket(t, 80, 2)
	a.Receive(whole[0], now.Add(ipAssemblyLifetime))
	if s := a.Snapshot(); s.PendingBytes != 0 || s.ExpiredPackets != 1 || s.WholePackets != 1 {
		t.Fatalf("whole traffic failed to expire old partial %+v", s)
	}
	a.Receive(d[1], now.Add(ipAssemblyLifetime))
	if a.Snapshot().PendingPackets != 0 || a.Snapshot().RetiredFragments != 1 {
		t.Fatal("expiry was renewed/resurrected")
	}
	a.Receive(d[0], now.Add(11*time.Second))
	if a.Snapshot().PendingBytes != 0 {
		t.Fatal("late first piece reopened expired ID")
	}
}

func TestIPAssemblyValidatesBeforeAllocationAndRetiresConflicts(t *testing.T) {
	now := time.Unix(100, 0)
	_, pieces := assemblyPacket(t, TunnelMTU, 1)
	for _, mutate := range []func([]byte){
		func(d []byte) { binary.BigEndian.PutUint16(d[5:7], TunnelMTU+1) },
		func(d []byte) { binary.BigEndian.PutUint16(d[7:9], 1) },
		func(d []byte) { binary.BigEndian.PutUint16(d[5:7], 19) },
		func(d []byte) { d[0] = PacketBabel },
	} {
		var a IPAssembler
		bad := bytes.Clone(pieces[0])
		mutate(bad)
		if _, err := a.Receive(bad, now); err == nil {
			t.Fatal("invalid header accepted")
		}
		if a.pending != nil || a.Snapshot().PendingBytes != 0 {
			t.Fatal("invalid header allocated partial storage")
		}
	}
	for _, changedLength := range []bool{false, true} {
		var a IPAssembler
		a.Receive(pieces[0], now)
		bad := bytes.Clone(pieces[0])
		if changedLength {
			binary.BigEndian.PutUint16(bad[5:7], TunnelMTU-1)
		} else {
			bad[100] ^= 1
		}
		if _, err := a.Receive(bad, now); err == nil {
			t.Fatal("conflicting duplicate accepted")
		}
		if s := a.Snapshot(); s.PendingBytes != 0 || s.ConflictingFragments != 1 {
			t.Fatalf("conflict did not retire reservation %+v", s)
		}
		a.Receive(pieces[1], now)
		if a.Snapshot().PendingBytes != 0 {
			t.Fatal("conflicted ID resurrected")
		}
	}
	// Changing an existing multi-piece ID to a complete short packet is a conflict.
	var a IPAssembler
	a.Receive(pieces[0], now)
	_, short := assemblyPacket(t, 80, 1)
	if _, err := a.Receive(short[0], now); err == nil || a.Snapshot().PendingBytes != 0 {
		t.Fatal("whole form bypassed conflicting partial")
	}
}

func TestIPAssemblySustainedLossRemainsBoundedAndDeliversFresh(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	for id := uint32(1); id <= 4096; id++ {
		want, d := assemblyPacket(t, TunnelMTU, id)
		a.Receive(d[id%2], now)
		if id%7 == 0 {
			got, err := a.Receive(d[1-id%2], now)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("fresh completion%d stalled: %v", id, err)
			}
		}
		checkAssemblyBounds(t, &a)
	}
	if s := a.Snapshot(); s.EvictedPackets == 0 || s.ReassembledPackets != 4096/7 {
		t.Fatalf("stress did not exercise progress %+v", s)
	}
	a.Expire(now.Add(ipAssemblyLifetime))
	checkAssemblyBounds(t, &a)
	if a.Snapshot().PendingBytes != 0 {
		t.Fatal("expired stress allocations retained")
	}
}

func TestIPAssemblySnapshotConcurrentAndIndependent(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	done := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				s := a.Snapshot()
				if s.PendingPackets < 0 || s.PendingPackets > ipPendingLimit || s.PendingBytes < 0 || s.PendingBytes > ipPendingByteLimit || s.PendingBytes != s.PendingPackets*TunnelMTU {
					t.Errorf("inconsistent concurrent copy %+v", s)
					return
				}
			}
		}()
	}
	initial := a.Snapshot()
	for id := uint32(1); id <= 1000; id++ {
		_, d := assemblyPacket(t, TunnelMTU, id)
		a.Receive(d[0], now)
		if id%2 == 0 {
			a.Receive(d[1], now)
		}
	}
	a.discard()
	close(done)
	readers.Wait()
	if initial.Fragments != 0 || initial.PendingBytes != 0 || a.Snapshot().DiscardedPackets == 0 {
		t.Fatal("snapshot alias or shutdown accounting")
	}
}

func TestIPAssemblyOwnedReceiveExpiryPreservesParent(t *testing.T) {
	var a IPAssembler
	_, d := assemblyPacket(t, TunnelMTU, 1)
	// Use the real lifetime: a short seeded deadline can expire before the
	// receiver starts, silently skipping the owned-deadline path under load.
	a.Receive(d[0], time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), ipAssemblyLifetime+5*time.Second)
	defer cancel()
	calls := 0
	got, err := a.receiveDatagram(ctx, func(actual context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			if actual == ctx {
				t.Fatal("receiver did not get an owned assembly deadline")
			}
			<-actual.Done()
			return nil, actual.Err()
		}
		if s := a.Snapshot(); s.PendingBytes != 0 || s.ExpiredPackets != 1 {
			t.Errorf("owner did not release expired data %+v", s)
		}
		return []byte{PacketBabel, 7}, nil
	})
	if err != nil || !bytes.Equal(got, []byte{PacketBabel, 7}) || calls != 2 || ctx.Err() != nil {
		t.Fatalf("expiry canceled healthy caller got=%v calls=%d err=%v parent=%v", got, calls, err, ctx.Err())
	}
	cancel()
	if _, err := a.receiveDatagram(ctx, func(actual context.Context) ([]byte, error) { <-actual.Done(); return nil, actual.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation hidden", err)
	}
}

func TestIPAssemblyParentCancellationAndDeadlineStayTerminal(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			var a IPAssembler
			_, pieces := assemblyPacket(t, TunnelMTU, 1)
			a.Receive(pieces[0], time.Now())
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			defer cancel()
			calls := 0
			_, err := a.receiveDatagram(ctx, func(actual context.Context) ([]byte, error) {
				calls++
				cancel()
				<-actual.Done()
				return nil, actual.Err()
			})
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("parent termination retried: calls=%d err=%v", calls, err)
			}
			a.discard()
		})
	}
}

func FuzzIPAssemblyBounds(f *testing.F) {
	_, ds := assemblyPacket(f, TunnelMTU, 1)
	seed := func(p []byte) []byte {
		b := make([]byte, 2+len(p))
		binary.BigEndian.PutUint16(b, uint16(len(p)))
		copy(b[2:], p)
		return b
	}
	f.Add(append(seed(ds[0]), seed(ds[1])...))
	f.Add(seed([]byte{PacketIP}))
	f.Fuzz(func(t *testing.T, data []byte) {
		a := &IPAssembler{}
		now := time.Unix(100, 0)
		for count := 0; count < 64 && len(data) >= 2; count++ {
			n := int(binary.BigEndian.Uint16(data[:2]))
			data = data[2:]
			if n > len(data) {
				n = len(data)
			}
			p, err := a.Receive(data[:n], now)
			data = data[n:]
			if p != nil && !validIP(p) {
				t.Fatal("partial/invalid IP delivered")
			}
			checkAssemblyBounds(t, a)
			if err != nil {
				a.discard()
				a = &IPAssembler{}
			}
			now = now.Add(time.Second)
		}
		a.Expire(now.Add(ipAssemblyLifetime))
		checkAssemblyBounds(t, a)
		if a.Snapshot().PendingBytes != 0 {
			t.Fatal("expired fuzz buffer retained")
		}
	})
}

func TestIPAssemblyRetiredReplayAllocatesNoBuffers(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	_, pieces := assemblyPacket(t, TunnelMTU, 7)
	a.Receive(pieces[0], now)
	a.Expire(now.Add(ipAssemblyLifetime))
	allocations := testing.AllocsPerRun(1000, func() { a.Receive(pieces[1], now.Add(ipAssemblyLifetime)) })
	if allocations != 0 || a.Snapshot().PendingBytes != 0 {
		t.Fatalf("late replay allocates: allocations=%v stats=%+v", allocations, a.Snapshot())
	}
	// Retirements are link-scoped. The same sequence can be used on a fresh link.
	var fresh IPAssembler
	fresh.Receive(pieces[1], now)
	if got, err := fresh.Receive(pieces[0], now); err != nil || got == nil {
		t.Fatal("fresh link inherited retirement", err)
	}
}

func TestIPAssemblySerialHalfRangeAndReorderedWhole(t *testing.T) {
	var a IPAssembler
	now := time.Unix(100, 0)
	for _, id := range []uint32{2000, 1999, 2001} {
		_, pieces := assemblyPacket(t, 80, id)
		if got, err := a.Receive(pieces[0], now); err != nil || got == nil {
			t.Fatalf("ordinary reordering ID%d rejected: %v", id, err)
		}
	}
	for _, id := range []uint32{2001 + (1 << 31), 2001 - ipRetirementWindow} {
		_, pieces := assemblyPacket(t, 80, id)
		if got, err := a.Receive(pieces[0], now); err != nil || got != nil {
			t.Fatalf("stale/ambiguous ID%d accepted: %v", id, err)
		}
	}
	if s := a.Snapshot(); s.WholePackets != 3 || s.RetiredFragments != 2 {
		t.Fatalf("unexpected serial counters %+v", s)
	}
}

func TestIPAssemblyOwnedDeadlineDoesNotHideTransportFailure(t *testing.T) {
	var a IPAssembler
	_, pieces := assemblyPacket(t, TunnelMTU, 1)
	a.Receive(pieces[0], time.Now())
	calls := 0
	_, err := a.receiveDatagram(context.Background(), func(ctx context.Context) ([]byte, error) {
		calls++
		if ctx.Err() != nil {
			t.Fatal("owned receive context already expired")
		}
		return nil, context.DeadlineExceeded // A transport error, not our deadline.
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatal("unrelated timeout was swallowed", err, calls)
	}
	a.discard()
}
