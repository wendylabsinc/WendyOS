package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

func TestCarrierFailureCannotBeMaskedByHealthyUplink(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("nan", []string{"NAN"})
	sharing := services.LocalMeshRuntimeStatus{Available: true, State: "local", Detail: "healthy independent uplink", AuthenticatedPeers: 2, GatewayAsset: 445}
	if state := h.status(sharing); state.State != "initializing" {
		t.Fatalf("before readiness: %+v", state)
	}
	h.report(epoch, "NAN", localmesh.CarrierStatus{Err: errors.New("ndi-create wnanndi0: FAIL")})
	state := h.status(sharing)
	if state.State != "failed" || !strings.Contains(state.Detail, "NAN: failed (ndi-create wnanndi0: FAIL)") || !strings.Contains(state.Detail, "Internet sharing: local (healthy independent uplink)") || state.AuthenticatedPeers != 2 || state.GatewayAsset != 445 {
		t.Fatalf("masked carrier failure: %+v", state)
	}
	// Neither an initializing callback nor another outer runtime attempt is
	// evidence of recovery. Only setup-complete readiness can clear failure.
	h.report(epoch, "NAN", localmesh.CarrierStatus{})
	epoch = h.begin("nan", []string{"NAN"})
	if state := h.status(sharing); state.State != "failed" {
		t.Fatalf("retry cleared error: %+v", state)
	}
	h.report(epoch, "NAN", localmesh.CarrierStatus{Ready: true})
	if state := h.status(sharing); state.State != "mesh-active" || strings.Contains(state.Detail, "FAIL") {
		t.Fatalf("not recovered: %+v", state)
	}
}

func TestCarrierPartialFailureTCPOnlyAndSharingError(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("tcp-nan", []string{"TCP", "NAN"})
	h.report(epoch, "TCP", localmesh.CarrierStatus{Ready: true})
	h.report(epoch, "NAN", localmesh.CarrierStatus{Err: errors.New("radio busy")})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "degraded" || !strings.Contains(state.Detail, "TCP: ready") {
		t.Fatalf("partial transport: %+v", state)
	}
	// Disabling every radio leaves configured TCP usable, not failed.
	epoch = h.begin("tcp-only", []string{"TCP"})
	h.report(epoch, "TCP", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" {
		t.Fatalf("TCP-only: %+v", state)
	}
	sharing := services.LocalMeshRuntimeStatus{Available: true, State: "error", Detail: "signed gateway expired", AuthenticatedPeers: 3, GatewayAsset: 358}
	state := h.status(sharing)
	if state.State != "degraded" || !strings.Contains(state.Detail, "signed gateway expired") || state.AuthenticatedPeers != 3 || state.GatewayAsset != 358 {
		t.Fatalf("sharing error hidden: %+v", state)
	}
}

func TestCarrierDisableRejectsStaleNotifications(t *testing.T) {
	var h meshCarrierHealth
	old := h.begin("nan", []string{"NAN"})
	h.report(old, "NAN", localmesh.CarrierStatus{Ready: true})
	h.end(old)
	h.report(old, "NAN", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.Available || state.State != "" {
		t.Fatalf("disabled status resurrected: %+v", state)
	}
	current := h.begin("ble", []string{"BLE"})
	h.report(old, "NAN", localmesh.CarrierStatus{Err: errors.New("late failure")})
	h.end(old)
	h.report(current, "BLE", localmesh.CarrierStatus{Ready: true})
	if state := h.status(services.LocalMeshRuntimeStatus{}); state.State != "mesh-active" || strings.Contains(state.Detail, "NAN") {
		t.Fatalf("old epoch overwrote new: %+v", state)
	}
}

func TestCarrierSupervisorWaitsForRealReadinessAndRejectsOldAttempt(t *testing.T) {
	var h meshCarrierHealth
	epoch := h.begin("nan", []string{"NAN"})
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
		runMeshCarrier(ctx, time.Millisecond, func(v localmesh.CarrierStatus) { h.report(epoch, "NAN", v); updates <- v }, func(ctx context.Context, report func(localmesh.CarrierStatus)) error {
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
