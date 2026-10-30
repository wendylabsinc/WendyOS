package btradio

import (
	"context"
	"os"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"path/filepath"
)

func resetPolicy(t *testing.T) {
	t.Helper()
	oldPath := policyPath
	policyPath = filepath.Join(t.TempDir(), "config", "radio.json")
	mu.Lock()
	loaded = false
	loadError = nil
	desired = &pb.BluetoothPowerSetting{Mode: "default"}
	handlers = map[string]*registration{}
	reports = map[string]*pb.BluetoothAdvertisingPowerStatus{}
	mu.Unlock()
	t.Cleanup(func() {
		policyPath = oldPath
		mu.Lock()
		loaded = false
		loadError = nil
		handlers = map[string]*registration{}
		reports = map[string]*pb.BluetoothAdvertisingPowerStatus{}
		mu.Unlock()
	})
}
func TestPolicyAppliesEveryAdvertisementAndSurvivesReload(t *testing.T) {
	resetPolicy(t)
	calls := map[string]int{}
	for _, name := range []string{"management", "wendy-net"} {
		Register(context.Background(), name, func(context.Context) error {
			calls[name]++
			p, err := Request(true, -40, 8)
			if err != nil {
				return err
			}
			Report(name, p, nil)
			return nil
		})
	}
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(Reports()) != 2 {
		t.Fatal(calls, Reports())
	}
	for _, report := range Reports() {
		if !report.Registered || report.GetRequestedDbm() != 8 || report.SelectedDbm != nil {
			t.Fatal(report)
		}
	}
	loaded = false
	got, err := Desired()
	if err != nil || got.Mode != "maximum" {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatal(err)
	}
}
func TestUnsupportedExplicitPolicyNeverFallsBack(t *testing.T) {
	resetPolicy(t)
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "dbm", Dbm: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := Request(false, 0, 0); err == nil {
		t.Fatal("unsupported explicit setting accepted")
	}
	if _, err := Request(true, -40, 8); err == nil {
		t.Fatal("out-of-range setting accepted")
	}
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "default"}); err != nil {
		t.Fatal(err)
	}
	if p, err := Request(false, 0, 0); p != nil || err != nil {
		t.Fatal(p, err)
	}
}
func TestPartialApplyIsVisibleAndSaved(t *testing.T) {
	resetPolicy(t)
	Register(context.Background(), "failing", func(context.Context) error {
		if p, _ := Desired(); p.Mode == "maximum" {
			return os.ErrPermission
		}
		return nil
	})
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err == nil {
		t.Fatal("lost application failure")
	}
	got, err := Desired()
	if err != nil || got.Mode != "maximum" {
		t.Fatal(got, err)
	}
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum", Dbm: 3}); err == nil {
		t.Fatal("invalid input persisted")
	}
}
func TestCorruptSavedPolicyIsReported(t *testing.T) {
	resetPolicy(t)
	if err := os.MkdirAll(filepath.Dir(policyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, []byte(`{"mode":"unknown"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Desired(); err == nil || !strings.Contains(err.Error(), "power") {
		t.Fatal(err)
	}
}
