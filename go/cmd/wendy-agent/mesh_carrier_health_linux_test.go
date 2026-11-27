//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func isolateNetCarrierRecovery(t *testing.T, err error) *int {
	t.Helper()
	previous := recoverNetCarrierState
	calls := 0
	recoverNetCarrierState = func(context.Context) error { calls++; return err }
	t.Cleanup(func() { recoverNetCarrierState = previous })
	return &calls
}

func TestCarrierRecoveryFailureStopsStartup(t *testing.T) {
	want := errors.New("owned firewall cleanup failed")
	calls := isolateNetCarrierRecovery(t, want)
	sharing := &meshSharingManager{}
	err := runConfiguredNetCarriers(context.Background(), t.TempDir(), localmesh.TCPIdentity{}, zap.NewNop(), nil, nil, sharing, nil)
	if !errors.Is(err, want) || *calls != 1 {
		t.Fatalf("recovery failure lost: calls=%d error=%v", *calls, err)
	}
	if state := sharing.Status(); state.Available || state.State != "" {
		t.Fatalf("started carriers after failed recovery: %+v", state)
	}
}

func TestCarrierStartupFailureAndConfigurationRemoval(t *testing.T) {
	isolateNetCarrierRecovery(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	if err := os.WriteFile(path, []byte(`{"nan":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nan-mesh.json"), []byte(`{"participate":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	sharing := &meshSharingManager{}
	// Missing enrollment cert fails before any kernel/network mutation.
	for range 2 {
		err := runConfiguredNetCarriers(context.Background(), dir, localmesh.TCPIdentity{}, zap.NewNop(), nil, nil, sharing, nil)
		if err == nil {
			t.Fatal("invalid identity was accepted")
		}
		state := sharing.Status()
		if state.State != "failed" || state.Carriers["nan"] == "" {
			t.Fatalf("startup failure masked: %+v", state)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := runConfiguredNetCarriers(context.Background(), dir, localmesh.TCPIdentity{}, zap.NewNop(), nil, nil, sharing, nil); err == nil {
		t.Fatal("invalid identity was accepted for default carriers")
	}
	if state := sharing.Status(); state.State != "failed" || state.Carriers[localmesh.CarrierEthernet] == "" || state.Carriers["ble"] == "" || state.Carriers["nan"] == "" {
		t.Fatalf("removed config did not restore the carrier defaults: %+v", state)
	}
}

func TestParticipationOverridesEnabledCarriersBeforeIdentityOrNetwork(t *testing.T) {
	isolateNetCarrierRecovery(t, nil)
	dir := t.TempDir()
	raw := []byte(`{"nan":true,"ble":true,"ethernet":true,"infrastructureWifi":true,"listen":"127.0.0.1:43023"}`)
	path := filepath.Join(dir, "local-mesh.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nan-mesh.json"), []byte(`{"participate":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	sharing := &meshSharingManager{}
	// An explicit opt-out blocks carriers before any identity or network setup.
	if err := runConfiguredNetCarriers(context.Background(), dir, localmesh.TCPIdentity{}, zap.NewNop(), nil, nil, sharing, nil); err != nil {
		t.Fatal(err)
	}
	if state := sharing.Status(); state.State != "disabled" || state.AuthenticatedPeers != 0 {
		t.Fatalf("disabled state: %+v", state)
	}
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != string(raw) {
		t.Fatalf("saved choices changed: %s %v", saved, err)
	}
}

func TestCarrierStartupFailureWithoutEnabledCarrierNames(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"unrecognizedCarrier":true}`} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "local-mesh.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			sharing := &meshSharingManager{}
			// Exercise the status initialization before invalid credentials stop
			// startup, without touching host routing or network interfaces.
			err := runEnabledMeshCarriers(context.Background(), dir, localmesh.TCPIdentity{}, zap.NewNop(), nil, nil, sharing, nil)
			if err == nil {
				t.Fatal("invalid startup accepted")
			}
			state := sharing.Status()
			if !state.Available || state.State != "failed" || state.Carriers["local-net"] == "" {
				t.Fatalf("startup failure hidden without named carriers: %+v", state)
			}
		})
	}
}
