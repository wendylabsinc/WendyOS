package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

func TestCarrierFailureCannotBeMaskedByHealthyUplink(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("nan", []string{"nan"})
	sharing := services.LocalMeshRuntimeStatus{Available: true, State: "local", Detail: "healthy independent uplink", AuthenticatedPeers: 2, GatewayPrincipal: "fixture-445"}
	if state := h.status(sharing); state.State != "initializing" {
		t.Fatalf("before readiness: %+v", state)
	}
	h.report(epoch, "nan", localmesh.CarrierStatus{Err: errors.New("ndi-create wnanndi0: FAIL")})
	state := h.status(sharing)
	if state.State != "failed" || !strings.Contains(state.Carriers["nan"], "fault (ndi-create wnanndi0: FAIL)") || !strings.Contains(state.Detail, "Internet sharing: local (healthy independent uplink)") || state.AuthenticatedPeers != 2 || state.GatewayPrincipal != "fixture-445" {
		t.Fatalf("masked carrier failure: %+v", state)
	}
	// Neither an initializing callback nor another outer runtime attempt is
	// evidence of recovery. Only setup-complete readiness can clear failure.
	h.report(epoch, "nan", localmesh.CarrierStatus{})
	epoch = h.begin("nan", []string{"nan"})
	if state := h.status(sharing); state.State != "failed" {
		t.Fatalf("retry cleared error: %+v", state)
	}
	h.report(epoch, "nan", localmesh.CarrierStatus{Ready: true})
	if state := h.status(sharing); state.State != "mesh-active" || strings.Contains(state.Carriers["nan"], "FAIL") {
		t.Fatalf("not recovered: %+v", state)
	}
}

func TestCarrierPartialFailureTCPOnlyAndSharingError(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("tcp-nan", []string{"tcp", "nan"})
	h.report(epoch, "tcp", localmesh.CarrierStatus{Ready: true})
	h.report(epoch, "nan", localmesh.CarrierStatus{Err: errors.New("radio busy")})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "degraded" || !strings.Contains(state.Carriers["tcp"], "ok") {
		t.Fatalf("partial transport: %+v", state)
	}
	// Disabling every radio leaves configured TCP usable, not failed.
	epoch = h.begin("tcp-only", []string{"tcp"})
	h.report(epoch, "tcp", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" {
		t.Fatalf("TCP-only: %+v", state)
	}
	sharing := services.LocalMeshRuntimeStatus{Available: true, State: "error", Detail: "signed gateway expired", AuthenticatedPeers: 3, GatewayPrincipal: "fixture-358"}
	state := h.status(sharing)
	if state.State != "degraded" || !strings.Contains(state.Detail, "signed gateway expired") || state.AuthenticatedPeers != 3 || state.GatewayPrincipal != "fixture-358" {
		t.Fatalf("sharing error hidden: %+v", state)
	}
}

func TestReadyBLEIsActiveWhileLANHasNoEligibleInterface(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("wifi-ble", []string{"ble", "ethernet"})
	sharing := services.LocalMeshRuntimeStatus{Available: true, State: "roaming", AuthenticatedPeers: 2, GatewayPrincipal: "verified-donor"}
	h.report(epoch, "ethernet", localmesh.CarrierStatus{Waiting: true, Detail: "waiting for an eligible LAN interface"})
	if state := h.status(sharing); state.State != "initializing" {
		t.Fatalf("no carrier is ready yet: %+v", state)
	}
	h.report(epoch, "ble", localmesh.CarrierStatus{Ready: true})
	state := h.status(sharing)
	if state.State != "mesh-active" || state.AuthenticatedPeers != 2 || state.GatewayPrincipal != sharing.GatewayPrincipal ||
		state.Carriers["ble"] != "ok" || state.Carriers["ethernet"] != "waiting (waiting for an eligible LAN interface)" {
		t.Fatalf("working BLE was hidden by a missing LAN interface: %+v", state)
	}
	// A real error is still degraded, even though the BLE data path works.
	h.report(epoch, "ethernet", localmesh.CarrierStatus{Err: errors.New("listener bind failed")})
	if state := h.status(sharing); state.State != "degraded" {
		t.Fatalf("LAN failure was hidden by ready BLE: %+v", state)
	}
}

func TestCarrierDisableRejectsStaleNotifications(t *testing.T) {
	var h meshCarrierHealth
	old := h.begin("nan", []string{"nan"})
	h.report(old, "nan", localmesh.CarrierStatus{Ready: true})
	h.end(old)
	h.report(old, "nan", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.Available || state.State != "" {
		t.Fatalf("disabled status resurrected: %+v", state)
	}
	current := h.begin("ble", []string{"ble"})
	h.report(old, "nan", localmesh.CarrierStatus{Err: errors.New("late failure")})
	h.end(old)
	h.report(current, "ble", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" || state.Carriers["nan"] != "" {
		t.Fatalf("old epoch overwrote new: %+v", state)
	}
}

func TestCarrierSupervisorWaitsForRealReadinessAndRejectsOldAttempt(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("nan", []string{"nan"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type attempt struct {
		report func(localmesh.CarrierStatus)
		result chan error
	}
	attempts := make(chan attempt, 2)
	updates := make(chan localmesh.CarrierStatus, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMeshCarrier(ctx, time.Millisecond, func(v localmesh.CarrierStatus) { h.report(epoch, "nan", v); updates <- v }, func(ctx context.Context, report func(localmesh.CarrierStatus)) error {
			a := attempt{report: report, result: make(chan error, 1)}
			attempts <- a
			select {
			case err := <-a.result:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	next := func() attempt {
		t.Helper()
		select {
		case a := <-attempts:
			return a
		case <-time.After(time.Second):
			t.Fatal("retry never started")
			return attempt{}
		}
	}
	first := next()
	first.result <- errors.New("two NDI capacity")
	select {
	case status := <-updates:
		if status.Err == nil {
			t.Fatal("failure not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("no failure notification")
	}
	second := next()
	first.report(localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "failed" {
		t.Fatalf("retry or old callback cleared failure: %+v", state)
	}
	second.report(localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" {
		t.Fatalf("real readiness ignored: %+v", state)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor failed to drain")
	}
}

func TestMissingCarrierHardwareIsTerminalAndNeutral(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("radios", []string{"ble", "nan", "ethernet"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, carrier := range []string{"ble", "nan"} {
		attempts := 0
		runMeshCarrier(ctx, time.Millisecond, func(v localmesh.CarrierStatus) {
			h.report(epoch, carrier, v)
		}, func(context.Context, func(localmesh.CarrierStatus)) error {
			attempts++
			return fmt.Errorf("%w: no adapter", localmesh.ErrNoCarrierHardware)
		})
		if attempts != 1 {
			t.Fatalf("%s no-hardware result retried %d times", carrier, attempts)
		}
	}
	h.report(epoch, "ethernet", localmesh.CarrierStatus{NoHardware: true})
	state := h.status(services.LocalMeshRuntimeStatus{})
	if state.State != "unavailable" || state.Carriers["ble"] != "no hardware (no adapter)" {
		t.Fatalf("missing hardware shown as failure: %+v", state)
	}
	h.report(epoch, "ethernet", localmesh.CarrierStatus{Waiting: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "waiting" {
		t.Fatalf("waiting for Ethernet: %+v", state)
	}
	h.report(epoch, "ethernet", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" {
		t.Fatalf("missing radios degraded Ethernet: %+v", state)
	}
}
