package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

const maxReceiptObservations = 420

type receiptObservation struct {
	Issued   int64 `json:"issued_at_unix_ms"`
	Expires  int64 `json:"expires_at_unix_ms"`
	Observed int64 `json:"observed_at_unix_ms"`
	Elapsed  int64 `json:"observed_elapsed_ns"`
}
type receiptObservations struct {
	Schema      string               `json:"schema"`
	RunID       string               `json:"run_id"`
	Operator    string               `json:"operator"`
	State       string               `json:"state"`
	Started     int64                `json:"started_at_unix_ms"`
	Finished    int64                `json:"finished_at_unix_ms"`
	ExitCode    int                  `json:"exit_code"`
	Receipts    []receiptObservation `json:"receipts"`
	Terminal    string               `json:"terminal_class"`
	Exchange    string               `json:"exchange"`
	HTTPStatus  int                  `json:"http_status"`
	Invalid     uint64               `json:"invalid_lines"`
	Truncated   bool                 `json:"truncated"`
	WriteFailed bool                 `json:"write_failed"`
}

type receiptCapture struct {
	mu                 sync.Mutex
	o                  options
	started            time.Time
	pending            []byte
	dropping           bool
	value              receiptObservations
	generation         uint64
	exportedGeneration uint64
}

func (c *receiptCapture) save(first bool) error {
	c.mu.Lock()
	if !first && c.generation == c.exportedGeneration {
		c.mu.Unlock()
		return nil
	}
	generation := c.generation
	b, err := json.Marshal(c.value)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if len(b) > 128<<10 {
		return errors.New("observation bound")
	}
	f, err := os.CreateTemp(filepath.Dir(c.o.observations), ".receipts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if first {
		err = os.Link(f.Name(), c.o.observations)
	} else {
		err = os.Rename(f.Name(), c.o.observations)
	}
	if err == nil {
		c.mu.Lock()
		c.exportedGeneration = generation
		c.mu.Unlock()
	}
	return err
}

var exchangeError = regexp.MustCompile(`^supervision (challenge|acknowledgement) exchange failed \((timeout|cancelled|transport or response|HTTP [1-5][0-9]{2})\)$`)

func (c *receiptCapture) line(b []byte) {
	now := time.Now()
	var lease client.SupervisionLease
	if len(b) > 0 && b[0] == '{' && authority.DecodeStrict(b, &lease) == nil && lease.Schema == "wendy.world.supervision-lease.v1" {
		if lease.Domain != "simulation" || lease.RunID != c.o.runID || lease.OperatorPrincipal != "lab/supervisor-"+c.o.operator || lease.GatewayPrincipal != "lab/node-1" || lease.TaskID != "load-mission" || lease.IssuedAtUnixMS <= 0 || lease.ExpiresAtUnixMS <= lease.IssuedAtUnixMS || lease.ExpiresAtUnixMS-lease.IssuedAtUnixMS > 500 {
			c.value.Invalid++
			return
		}
		if len(c.value.Receipts) >= maxReceiptObservations {
			c.value.Truncated = true
			return
		}
		c.value.Receipts = append(c.value.Receipts, receiptObservation{lease.IssuedAtUnixMS, lease.ExpiresAtUnixMS, now.UnixMilli(), now.Sub(c.started).Nanoseconds()})
		return
	}
	text := string(b)
	if m := exchangeError.FindStringSubmatch(text); m != nil {
		c.value.Exchange = m[1]
		switch m[2] {
		case "timeout", "cancelled":
			c.value.Terminal = m[2]
		case "transport or response":
			c.value.Terminal = "transport_or_response"
		default:
			c.value.Terminal = "http_refusal"
			c.value.HTTPStatus = int(m[2][5]-'0')*100 + int(m[2][6]-'0')*10 + int(m[2][7]-'0')
		}
		return
	}
	switch text {
	case "supervision diagnostics export failed; supervision continues":
		c.value.Terminal = "diagnostics_export_failed"
	case "supervisor authority no longer valid", "untrusted or expired supervision challenge":
		c.value.Terminal = "authority_or_challenge_expired"
	case "supervision challenge deadline exceeded", "supervision receipt arrived after exchange deadline":
		c.value.Terminal = "exchange_deadline"
	case "untrusted, stale or mismatched supervision receipt":
		c.value.Terminal = "receipt_refused"
	case "gateway boot or executor generation changed; explicit restart required":
		c.value.Terminal = "generation_changed"
	default:
		var ended struct {
			Schema, Reason string
			Acknowledged   uint64 `json:"acknowledged_leases"`
			RobotState     string `json:"robot_state"`
		}
		if json.Unmarshal(b, &ended) == nil && ended.Schema == "wendy.world.supervision-ended.v1" {
			c.value.Terminal = "local_duration_or_cancellation"
		} else {
			c.value.Invalid++
		}
	}
}

// Never propagate observer I/O failure into the otherwise unchanged control
// process. The final report is incomplete; supervision retains its own limits.
func (c *receiptCapture) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	original := len(b)
	for len(b) > 0 {
		newline := bytes.IndexByte(b, '\n')
		part := b
		if newline >= 0 {
			part = b[:newline]
		}
		if !c.dropping {
			if len(c.pending)+len(part) > 4096 {
				c.value.Truncated = true
				c.generation++
				c.dropping = true
				c.pending = nil
			} else {
				c.pending = append(c.pending, part...)
			}
		}
		if newline < 0 {
			break
		}
		if !c.dropping {
			c.line(c.pending)
			c.generation++
		}
		c.pending = nil
		c.dropping = false
		b = b[newline+1:]
	}
	return original, nil
}

func captureSupervisor(ctx context.Context, o options) error {
	start := time.Now()
	c := &receiptCapture{o: o, started: start, value: receiptObservations{Schema: "wendy.world.supervision-receipts.v1", RunID: o.runID, Operator: o.operator, State: "running", Started: start.UnixMilli(), ExitCode: -1, Receipts: []receiptObservation{}}}
	if err := c.save(true); err != nil {
		return errors.New("receipt observation destination refused")
	}
	// Fixed command only; input scope was validated by the gate before release.
	childCtx, cancel := context.WithTimeout(ctx, 42*time.Second)
	defer cancel()
	child := exec.CommandContext(childCtx, "/world", command(o)[1:]...)
	child.Stdout = c
	child.Stderr = c
	child.WaitDelay = time.Second
	exportCtx, stopExport := context.WithCancel(context.Background())
	exported := make(chan struct{})
	go func() {
		defer close(exported)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-exportCtx.Done():
				return
			case <-ticker.C:
				if saveErr := c.save(false); saveErr != nil {
					c.mu.Lock()
					c.value.WriteFailed = true
					c.generation++
					c.mu.Unlock()
				}
			}
		}
	}()
	err := child.Run()
	stopExport()
	<-exported
	c.mu.Lock()
	c.value.State = "exited"
	c.generation++
	c.value.Finished = time.Now().UnixMilli()
	if child.ProcessState != nil {
		c.value.ExitCode = child.ProcessState.ExitCode()
	} else {
		c.value.Terminal = "child_start_failed"
	}
	if len(c.pending) > 0 {
		c.line(c.pending)
		c.pending = nil
	}
	c.mu.Unlock()
	if saveErr := c.save(false); saveErr != nil {
		c.mu.Lock()
		c.value.WriteFailed = true
		c.mu.Unlock()
	}
	if err != nil {
		return errors.New("supervisor child failed")
	}
	return nil
}

var _ io.Writer = (*receiptCapture)(nil)
