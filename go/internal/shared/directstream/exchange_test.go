package directstream

import (
	"crypto/sha256"
	"testing"
)

func exchangeConfig(role byte, initial bool, applied *int) ExchangeConfig {
	g := NewPendingGenerationGate()
	if !initial {
		g, _ = NewGenerationGate(1, 1, DirectBLE)
		_ = g.Frozen(1)
	}
	cfg := ExchangeConfig{Binding: SessionBinding{SessionID: [32]byte{1}, Capability: [32]byte{2}, IdentityDigest: [32]byte{3}, Role: role}, Gate: g, Token: uint64(10 + role), Nonce: [16]byte{4 + role}, MaxRemoteRecords: 8,
		Export: func(label string, context []byte, n int) ([]byte, error) {
			h := sha256.Sum256(append([]byte(label), context...))
			return h[:], nil
		},
		Apply: func() error { *applied++; return nil }}
	if !initial {
		cfg.Local.Namespaces = [2]NamespaceCheckpoint{{HighWater: 10, Admitted: 5, Grant: 8}, {Grant: 8}}
		for id := uint32(2); id <= 10; id += 2 {
			cfg.Local.Records = append(cfg.Local.Records, RecoveryRecord{ID: id, OpenComplete: true, OpenDigest: [32]byte{byte(id)}})
		}
	}
	return cfg
}
func exchangeWire(t *testing.T, e *AttachmentExchange) ControlRecord {
	t.Helper()
	r, ok := e.Next()
	if !ok {
		t.Fatal("missing outbound")
	}
	// Backpressure does not advance the driver or require new crypto.
	again, _ := e.Next()
	if again.Kind != r.Kind || again.AttachmentID != r.AttachmentID {
		t.Fatal("unstable queue")
	}
	var buf [MaxFlowFrame]byte
	n, err := r.Encode(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControlRecord(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Accepted(); err != nil {
		t.Fatal(err)
	}
	return got
}
func TestAttachmentExchangeWireAndDuplicateCommit(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial", false: "five-flow recovery"}[initial], func(t *testing.T) {
			calls0, calls1 := 0, 0
			c0, c1 := exchangeConfig(0, initial, &calls0), exchangeConfig(1, initial, &calls1)
			generation := uint64(1)
			if !initial {
				generation = 2
			}
			c, err := NewCoordinatorExchange(c0, 7, generation, DirectBLE, initial)
			if err != nil {
				t.Fatal(err)
			}
			p, err := NewParticipantExchange(c1, exchangeWire(t, c))
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, ok := c.Next(); !ok {
					break
				}
				if err = p.Receive(exchangeWire(t, c)); err != nil {
					t.Fatal(err)
				}
			}
			for {
				if _, ok := p.Next(); !ok {
					break
				}
				if err = c.Receive(exchangeWire(t, p)); err != nil {
					t.Fatal(err)
				}
			}
			if c.Active() || p.Active() || calls0 != 1 || calls1 != 0 {
				t.Fatal("premature activation/application")
			}
			commit := exchangeWire(t, c)
			if err = p.Receive(commit); err != nil {
				t.Fatal(err)
			}
			if p.Active() || calls1 != 1 {
				t.Fatal("active before writer owns ACK")
			}
			ack := exchangeWire(t, p)
			if !p.Active() {
				t.Fatal("participant not active")
			}
			if err = c.Receive(ack); err != nil || !c.Active() {
				t.Fatal(err)
			}
			// Both endpoint buffers may now have advanced; these messages must replay
			// recorded commit state and never reapply the old recovery plan.
			if err = p.Receive(commit); err != nil {
				t.Fatal(err)
			}
			if err = c.Receive(exchangeWire(t, p)); err != nil {
				t.Fatal(err)
			}
			if err = c.Receive(ack); err != nil {
				t.Fatal(err)
			}
			if calls0 != 1 || calls1 != 1 {
				t.Fatal("reapplied snapshot")
			}
		})
	}
}
func TestAttachmentExchangeCutEveryRecord(t *testing.T) {
	for cut := 0; cut < 6; cut++ {
		n0, n1 := 0, 0
		c0, c1 := exchangeConfig(0, true, &n0), exchangeConfig(1, true, &n1)
		c, _ := NewCoordinatorExchange(c0, 7, 1, DirectBLE, true)
		var p *AttachmentExchange
		for step := 0; step < cut; step++ {
			switch step {
			case 0:
				var err error
				p, err = NewParticipantExchange(c1, exchangeWire(t, c))
				if err != nil {
					t.Fatal(err)
				}
			case 1, 2:
				if err := c.Receive(exchangeWire(t, p)); err != nil {
					t.Fatal(err)
				}
			case 3:
				if err := p.Receive(exchangeWire(t, c)); err != nil {
					t.Fatal(err)
				}
			case 4:
				if err := c.Receive(exchangeWire(t, p)); err != nil {
					t.Fatal(err)
				}
			}
		}
		c.Lost()
		if p != nil {
			p.Lost()
		}
		if c0.Gate.Phase() != AttachmentSuspended {
			t.Fatal("direct loss terminal")
		}
		if next, ok := c0.Gate.NextGeneration(); !ok || next != 2 {
			t.Fatal("reused offer generation")
		}
		if p != nil && c1.Gate.Phase() != AttachmentSuspended {
			t.Fatal("participant loss terminal")
		}
	}
}

func TestEmptyCreationRetryAfterLostOfferOrReady(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		n0, n1 := 0, 0
		c0, c1 := exchangeConfig(0, true, &n0), exchangeConfig(1, true, &n1)
		c, err := NewCoordinatorExchange(c0, 1, 1, DirectBLE, true)
		if err != nil {
			t.Fatal(err)
		}
		offer := exchangeWire(t, c)
		var p *AttachmentExchange
		if delivered {
			p, err = NewParticipantExchange(c1, offer)
			if err != nil {
				t.Fatal(err)
			}
		}
		c.Lost()
		if p != nil {
			p.Lost()
		}
		c0.Token++
		c1.Token++
		c0.Nonce[0] += 10
		c1.Nonce[0] += 10
		c, err = NewCoordinatorExchange(c0, 1, 2, DirectBLE, true)
		if err != nil {
			t.Fatal("retry creation", err)
		}
		p, err = NewParticipantExchange(c1, exchangeWire(t, c))
		if err != nil {
			t.Fatal("retry offer", err)
		}
		for {
			if _, ok := p.Next(); !ok {
				break
			}
			if err = c.Receive(exchangeWire(t, p)); err != nil {
				t.Fatal(err)
			}
		}
		if err = p.Receive(exchangeWire(t, c)); err != nil {
			t.Fatal(err)
		}
		if err = c.Receive(exchangeWire(t, p)); err != nil {
			t.Fatal(err)
		}
		if !c.Active() || !p.Active() || n0 != 1 || n1 != 1 {
			t.Fatal("initial retry not committed exactly once")
		}
	}
}
