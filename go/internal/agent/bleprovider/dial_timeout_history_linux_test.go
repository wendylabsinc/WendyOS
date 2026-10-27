//go:build linux

package bleprovider

import (
	"fmt"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDialTimeoutHistoryBoundsUntrustedAddresses(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	r := &runtime{cfg: Config{Logger: zap.New(core)}}
	for i := range maxPeerRetryHistory + 100 {
		r.noteDialTimeout(fmt.Sprintf("radio-%d", i), true)
	}
	if len(r.dialTimeouts) != maxPeerRetryHistory {
		t.Fatalf("untrusted radio history grew to %d", len(r.dialTimeouts))
	}
	// A full history must still track existing peers and emit one warning
	// per consecutive failure episode, without an unbounded counter.
	for range 10 {
		r.noteDialTimeout("radio-0", true)
	}
	if logs.Len() != 1 || r.dialTimeouts["radio-0"] != 3 {
		t.Fatalf("warnings=%d counter=%d", logs.Len(), r.dialTimeouts["radio-0"])
	}
	r.noteDialTimeout("radio-0", false)
	r.noteDialTimeout("new-radio", true)
	if len(r.dialTimeouts) != maxPeerRetryHistory || r.dialTimeouts["new-radio"] != 1 {
		t.Fatal("successful dial did not release diagnostic capacity")
	}
	r.clearDialTimeouts("new-radio")
	if _, exists := r.dialTimeouts["new-radio"]; exists {
		t.Fatal("explicit reset retained timeout history")
	}
}
