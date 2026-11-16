package directstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAttachmentExchangeLiteInterop(t *testing.T) {
	binaryPath := os.Getenv("WENDY_DIRECT_EXCHANGE_PEER")
	if binaryPath == "" {
		t.Skip("requires compiled Lite direct_exchange_peer")
	}
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial", false: "recovery"}[initial], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			arg := "--pipe-initial"
			if !initial {
				arg = "--pipe-recovery"
			}
			cmd := exec.CommandContext(ctx, binaryPath, arg)
			cmd.Stderr = os.Stderr
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { input.Close(); cancel(); cmd.Wait() }()
			calls := 0
			cfg := exchangeConfig(0, initial, &calls)
			cfg.Export = func(string, []byte, int) ([]byte, error) { return bytes.Repeat([]byte{6}, 32), nil }
			generation := uint64(1)
			if !initial {
				generation = 2
			}
			c, err := NewCoordinatorExchange(cfg, 7, generation, DirectBLE, initial)
			if err != nil {
				t.Fatal(err)
			}
			var lastCommit ControlRecord
			send := func(r ControlRecord) {
				t.Helper()
				var b [MaxFlowFrame]byte
				n, err := r.Encode(b[:])
				if err != nil {
					t.Fatal(err)
				}
				var prefix [2]byte
				binary.BigEndian.PutUint16(prefix[:], uint16(n))
				if _, err = input.Write(prefix[:]); err != nil {
					t.Fatal(err)
				}
				if _, err = input.Write(b[:n]); err != nil {
					t.Fatal(err)
				}
			}
			receive := func() ControlRecord {
				t.Helper()
				var prefix [2]byte
				if _, err := io.ReadFull(output, prefix[:]); err != nil {
					t.Fatalf("read: %v", err)
				}
				n := int(binary.BigEndian.Uint16(prefix[:]))
				if n > MaxFlowFrame {
					t.Fatal("unbounded peer output")
				}
				b := make([]byte, n)
				if _, err := io.ReadFull(output, b); err != nil {
					t.Fatal(err)
				}
				r, err := DecodeControlRecord(b)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			for !c.Active() {
				for {
					if _, ok := c.Next(); !ok {
						break
					}
					r := exchangeWire(t, c)
					if r.Kind == ControlCommit {
						lastCommit = r
					}
					send(r)
				}
				if err = c.Receive(receive()); err != nil {
					t.Fatal(err)
				}
			}
			send(lastCommit)
			if err = c.Receive(receive()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("reapplied Go plan")
			}
			input.Close()
			if err = cmd.Wait(); err != nil {
				t.Fatalf("C peer: %v", err)
			}
		})
	}
}
