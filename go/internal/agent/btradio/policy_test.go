package btradio

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"path/filepath"
)

func resetPolicy(t *testing.T) {
	t.Helper()
	oldPath := policyPath
	policyPath = filepath.Join(t.TempDir(), "config", "radio.json")
	mu.Lock()
	loaded = false
	meshMode = ""
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

func TestRadioProfileAndPowerPreserveEachOtherOnReload(t *testing.T) {
	resetPolicy(t)
	if err := SetRadio(context.Background(), nil, "coded-s2"); err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "dbm", Dbm: -6}); err != nil {
		t.Fatal(err)
	}
	loaded = false
	meshMode = ""
	if mode, err := MeshMode(); err != nil || mode != "coded-s2" {
		t.Fatal(mode, err)
	}
	if power, err := Desired(); err != nil || power.Mode != "dbm" || power.Dbm != -6 {
		t.Fatal(power, err)
	}
	if err := SetRadio(context.Background(), nil, "normal"); err != nil {
		t.Fatal(err)
	}
	if power, _ := Desired(); power.Dbm != -6 {
		t.Fatal(power)
	}
}

func TestPowerOnlyPolicyDoesNotSelectMeshPHY(t *testing.T) {
	resetPolicy(t)
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err != nil {
		t.Fatal(err)
	}
	loaded = false
	if MeshModeConfigured() {
		t.Fatal("advertising power must not change discovery PHY policy")
	}
	if mode, _ := MeshMode(); mode != "normal" {
		t.Fatal(mode)
	}
}

func TestRadioEffectsCannotBeOvertakenByNewPolicy(t *testing.T) {
	resetPolicy(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	second := make(chan error, 1)
	var liveMode string
	go func() {
		first <- SetRadioAndApply(context.Background(), nil, "coded-s2", func(context.Context) error {
			close(entered)
			<-release
			liveMode = "coded-s2"
			return nil
		})
	}()
	<-entered
	started := make(chan struct{})
	go func() {
		close(started)
		second <- SetRadioAndApply(context.Background(), nil, "normal", func(context.Context) error {
			liveMode = "normal"
			return nil
		})
	}()
	<-started
	var overtook bool
	select {
	case <-second:
		overtook = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !overtook {
		if err := <-second; err != nil {
			t.Fatal(err)
		}
	}
	mode, err := MeshMode()
	if overtook || err != nil || mode != "normal" || liveMode != mode {
		t.Fatalf("new policy overtook pending effects: overtook=%t saved=%s live=%s err=%v", overtook, mode, liveMode, err)
	}
}

func TestRadioEffectFailureRetainsSavedIntent(t *testing.T) {
	resetPolicy(t)
	failure := errors.New("controller rejected PHY")
	err := SetRadioAndApply(context.Background(), nil, "coded-s8", func(context.Context) error { return failure })
	var applied *ApplyError
	if !errors.As(err, &applied) || !errors.Is(err, failure) {
		t.Fatalf("effect failure lost saved-policy semantics: %v", err)
	}
	if mode, err := MeshMode(); err != nil || mode != "coded-s8" {
		t.Fatal(mode, err)
	}
}
