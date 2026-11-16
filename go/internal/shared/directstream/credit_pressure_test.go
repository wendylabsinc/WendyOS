package directstream

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// Two stopped readers must not consume the progress reservation of an already
// admitted interactive stream as that stream returns chunks to the shared pool.
func TestReliableFlowsStoppedReadersPreserveInteractiveCredit(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	var clients, servers []*RetainedConn
	opened := func(m *ReliableFlows, e IncomingOpen) {
		c, err := m.CompleteIncoming(e.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		servers = append(servers, c)
	}
	for i := 0; i < 3; i++ {
		id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: uint16(28080 + i)}, now)
		if err != nil {
			t.Fatal(err)
		}
		pumpFlows(t, a, b, &now, opened)
		c, err := a.Connection(id)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	payload := bytes.Repeat([]byte{0xa5}, 512)
	for i := 0; i < 8; i++ {
		for j := 0; j < 2; j++ {
			if _, err := clients[j].Write(payload); err != nil {
				t.Fatal(err)
			}
			pumpFlows(t, a, b, &now, nil)
		}
	}
	// Free one stream's entire minimum share, then race a new admission
	// against its refill. Physical free space is not unowned capacity.
	probe := bytes.Repeat([]byte{0x37}, 1024)
	if _, err := clients[2].Write(probe); err != nil {
		t.Fatal(err)
	}
	pumpFlows(t, a, b, &now, nil)
	servers[2].SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	got := make([]byte, len(probe))
	if _, err := io.ReadFull(servers[2], got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, probe) {
		t.Fatal("probe payload changed")
	}
	if b.budget.rx.Available() < 1024 {
		t.Fatal("fixture did not expose recycled capacity")
	}
	if _, err := b.Begin(OpenRequest{Service: ServiceTCP, Port: 29000}, now); err != ErrCapacity {
		t.Fatalf("new admission stole a live progress reservation: %v", err)
	}
	pumpFlows(t, a, b, &now, nil)
	recoverFlowPair(t, a, b, &now)
	pumpFlows(t, a, b, &now, nil)
	for round := 0; round < 16; round++ {
		if _, err := clients[2].Write(payload); err != nil {
			t.Fatal(err)
		}
		pumpFlows(t, a, b, &now, nil)
		servers[2].SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(servers[2], got); err != nil {
			t.Fatalf("interactive round%d starved: %v; receive pool free%d", round, err, b.budget.rx.Available())
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("payload changed")
		}
		pumpFlows(t, a, b, &now, nil)
	}
	for j := 0; j < 2; j++ {
		for i := 0; i < 8; i++ {
			servers[j].SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(servers[j], got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("stopped reader payload changed")
			}
			pumpFlows(t, a, b, &now, nil)
		}
	}
	a.Close(io.ErrClosedPipe)
	b.Close(io.ErrClosedPipe)
	if a.budget.rx.Available() != 8192 || a.budget.tx.Available() != 8192 || b.budget.rx.Available() != 8192 || b.budget.tx.Available() != 8192 {
		t.Fatal("pool leak")
	}
}

func TestReliableFlowsBlockedWritersPreserveInteractiveSend(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	var clients, servers []*RetainedConn
	opened := func(m *ReliableFlows, e IncomingOpen) {
		c, err := m.CompleteIncoming(e.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		servers = append(servers, c)
	}
	for i := 0; i < 3; i++ {
		id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: uint16(28100 + i)}, now)
		if err != nil {
			t.Fatal(err)
		}
		pumpFlows(t, a, b, &now, opened)
		c, err := a.Connection(id)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	// No carrier progress: bulk writers can retain data but must leave the
	// already admitted third writer enough space for a small request.
	payload := bytes.Repeat([]byte{0x6d}, 4096)
	accepted := make([]int, 2)
	for i := 0; i < 2; i++ {
		clients[i].SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := clients[i].Write(payload)
		if err != nil && !os.IsTimeout(err) {
			t.Fatal(err)
		}
		accepted[i] = n
	}
	request := []byte("interactive during suspension")
	clients[2].SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := clients[2].Write(request); err != nil || n != len(request) {
		t.Fatalf("interactive writer starved: n%d error%v", n, err)
	}
	recoverFlowPair(t, a, b, &now)
	pumpFlows(t, a, b, &now, nil)
	servers[2].SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(request))
	if _, err := io.ReadFull(servers[2], got); err != nil || !bytes.Equal(got, request) {
		t.Fatalf("request after recovery: %q %v", got, err)
	}
	for i := 0; i < 2; i++ {
		for left := accepted[i]; left > 0; {
			n := min(left, 512)
			got := make([]byte, n)
			servers[i].SetReadDeadline(time.Now().Add(time.Second))
			if _, err := io.ReadFull(servers[i], got); err != nil || !bytes.Equal(got, payload[:n]) {
				t.Fatalf("bulk suffix: %v", err)
			}
			left -= n
			pumpFlows(t, a, b, &now, nil)
		}
	}
	a.Close(io.ErrClosedPipe)
	b.Close(io.ErrClosedPipe)
	if a.budget.senders != nil || b.budget.senders != nil || a.budget.tx.Available() != 8192 || b.budget.tx.Available() != 8192 {
		t.Fatal("TX pool leak")
	}
}

func TestManagedSendShareRetiresOnPromotionAndClose(t *testing.T) {
	b, err := NewConnBudget(1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.newConn(1024, 256, nil, nil, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.newConn(1024, 256, nil, nil, 256, nil); err != ErrCapacity {
		t.Fatalf("over-admitted send minimum: %v", err)
	}
	raw, remote := promotionPair()
	defer remote.Close()
	defer c.Close()
	done, err := c.CommitPromotion(raw)
	if err != nil {
		t.Fatal(err)
	}
	promotionWait(t, done)
	b.mu.Lock()
	retired := b.senders == nil
	b.mu.Unlock()
	if !retired {
		t.Fatal("promotion retained sender registration")
	}
	next, err := b.newConn(1024, 256, nil, nil, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	c.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.senders != nil || b.tx.Available() != 1024 || b.rx.Available() != 4096 {
		t.Fatal("closed send shares or buffers retained")
	}
}

// Admission and minimum-progress shares stay small, but a consuming reader
// should be able to use its configured window when the shared pool has room.
func TestReliableFlowsBusyReceiveWindowGrowsBeyondAdmission(t *testing.T) {
	a, b := flowPair(t)
	var now int64
	pumpFlows(t, a, b, &now, nil)
	var receiver *RetainedConn
	id, err := a.Begin(OpenRequest{Service: ServiceTCP, Port: 28123}, now)
	if err != nil {
		t.Fatal(err)
	}
	pumpFlows(t, a, b, &now, func(m *ReliableFlows, e IncomingOpen) {
		receiver, err = m.CompleteIncoming(e.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
	})
	sender, err := a.Connection(id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := b.flows[id].lane.Snapshot()
	if err != nil || before.Limit != 1024 {
		t.Fatalf("admission changed: %+v %v", before, err)
	}
	data := bytes.Repeat([]byte{0x42}, 512)
	for round := 0; round < 16; round++ {
		if _, err = sender.Write(data); err != nil {
			t.Fatal(err)
		}
		pumpFlows(t, a, b, &now, nil)
		receiver.SetReadDeadline(time.Now().Add(time.Second))
		got := make([]byte, len(data))
		if _, err = io.ReadFull(receiver, got); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("payload: %v", err)
		}
		pumpFlows(t, a, b, &now, nil)
	}
	after, err := b.flows[id].lane.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	outstanding := after.Limit - after.Consumed
	if outstanding <= 1024 || outstanding > 4096 {
		t.Fatalf("working credit%d did not grow within window", outstanding)
	}
}
