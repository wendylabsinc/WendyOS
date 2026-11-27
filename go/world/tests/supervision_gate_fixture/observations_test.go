package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func captureFixture(t *testing.T) *receiptCapture {
	t.Helper()
	return &receiptCapture{o: options{runID: "run", operator: "a", observations: filepath.Join(t.TempDir(), "receipt.json")}, started: time.Now(), value: receiptObservations{Schema: "wendy.world.supervision-receipts.v1", RunID: "run", Operator: "a", State: "running", Receipts: []receiptObservation{}}}
}
func TestReceiptCaptureUsesCompletionObservationAndDropsPrivateFields(t *testing.T) {
	c := captureFixture(t)
	if c.save(true) != nil {
		t.Fatal("initial save")
	}
	now := time.Now().UnixMilli()
	lease := client.SupervisionLease{Schema: "wendy.world.supervision-lease.v1", Domain: "simulation", RunID: "run", OperatorPrincipal: "lab/supervisor-a", GatewayPrincipal: "lab/node-1", TaskID: "load-mission", IssuedAtUnixMS: now - 50, ExpiresAtUnixMS: now + 450, ChallengeID: "private-proof-marker"}
	line, _ := json.Marshal(lease)
	line = append(line, '\n')
	_, _ = c.Write(line[:8])
	_, _ = c.Write(line[8:])
	if len(c.value.Receipts) != 1 || c.value.Receipts[0].Observed < now || c.value.Receipts[0].Issued != now-50 {
		t.Fatal(c.value)
	}
	out, _ := json.Marshal(c.value)
	if strings.Contains(string(out), "private-proof-marker") || strings.Contains(string(out), "operator_principal") {
		t.Fatal("raw CLI fields copied")
	}
	if c.save(false) != nil {
		t.Fatal("save")
	}
}
func TestReceiptCaptureRejectsWrongScopeAndBoundsUnknownOutput(t *testing.T) {
	c := captureFixture(t)
	_, _ = c.Write([]byte(strings.Repeat("secret", 2000) + "\n"))
	_, _ = c.Write([]byte("supervision challenge exchange failed (timeout)\nsecret-response\n"))
	if !c.value.Truncated || c.value.Terminal != "timeout" || c.value.Invalid != 1 {
		t.Fatal(c.value)
	}
	now := time.Now().UnixMilli()
	lease := client.SupervisionLease{Schema: "wendy.world.supervision-lease.v1", Domain: "physical", RunID: "run", OperatorPrincipal: "lab/supervisor-a", GatewayPrincipal: "lab/node-1", TaskID: "load-mission", IssuedAtUnixMS: now, ExpiresAtUnixMS: now + 500}
	line, _ := json.Marshal(lease)
	_, _ = c.Write(append(line, '\n'))
	if len(c.value.Receipts) != 0 || c.value.Invalid != 2 {
		t.Fatal(c.value)
	}
	lease.Domain = "simulation"
	line, _ = json.Marshal(lease)
	for i := 0; i < maxReceiptObservations+2; i++ {
		_, _ = c.Write(append(line, '\n'))
	}
	if len(c.value.Receipts) != maxReceiptObservations {
		t.Fatal("unbounded receipts")
	}
}
func TestReceiptOutputRefusesExistingDestination(t *testing.T) {
	c := captureFixture(t)
	if c.save(true) != nil {
		t.Fatal("save")
	}
	if c.save(true) == nil {
		t.Fatal("overwrote destination")
	}
}

func TestReceiptExporterSkipsUnchangedGenerations(t *testing.T) {
	c := captureFixture(t)
	if c.save(true) != nil {
		t.Fatal("initial write")
	}
	first, err := os.Stat(c.o.observations)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if c.save(false) != nil {
			t.Fatal("unchanged save")
		}
	}
	unchanged, _ := os.Stat(c.o.observations)
	if !os.SameFile(first, unchanged) {
		t.Fatal("unchanged snapshot rewritten")
	}
	_, _ = c.Write([]byte("supervision challenge exchange failed (timeout)\n"))
	if c.save(false) != nil {
		t.Fatal("dirty save")
	}
	changed, _ := os.Stat(c.o.observations)
	if os.SameFile(first, changed) {
		t.Fatal("changed snapshot not exported")
	}
	if c.save(false) != nil {
		t.Fatal("stable save")
	}
	repeated, _ := os.Stat(c.o.observations)
	if !os.SameFile(changed, repeated) {
		t.Fatal("dirty generation not acknowledged")
	}
}
