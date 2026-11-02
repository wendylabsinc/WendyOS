//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"fmt"
	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"testing"
)

func TestNormalAlreadyStoppedDiscoveryRecoversReadiness(t *testing.T) {
	absent := dbus.NewError("org.bluez.Error.Failed", []any{"No discovery started"})
	inProgress := dbus.NewError("org.bluez.Error.InProgress", []any{"Operation already in progress"})
	var statuses []localmesh.CarrierStatus
	r := &runtime{cfg: Config{Status: func(s localmesh.CarrierStatus) { statuses = append(statuses, s) }}}
	r.reportDiscoveryHealth(context.Background(), inProgress)
	var calls []string
	err := restartDiscoverySession(context.Background(), func(_ context.Context, method string, args ...any) error {
		calls = append(calls, method)
		switch method {
		case adapterInterface + ".StopDiscovery":
			return absent
		case adapterInterface + ".SetDiscoveryFilter":
			filter := args[0].(map[string]dbus.Variant)
			if filter["Transport"].Value() != "le" || filter["DuplicateData"].Value() != true {
				t.Fatal("filter changed")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{adapterInterface + ".StopDiscovery", adapterInterface + ".SetDiscoveryFilter", adapterInterface + ".StartDiscovery"}
	if fmt.Sprint(calls) != fmt.Sprint(expected) {
		t.Fatalf("calls %v", calls)
	}
	r.reportDiscoveryHealth(context.Background(), err)
	if len(statuses) != 2 || statuses[0].Err == nil || statuses[1].Err != nil || !statuses[1].Ready || r.discoveryErr != nil {
		t.Fatalf("readiness %+v", statuses)
	}
	for _, e := range []error{absent, *absent, fmt.Errorf("stopping caller session: %w", *absent)} {
		if stoppedDiscoveryResult(e) != nil {
			t.Fatal("already stopped not idempotent")
		}
	}
	if !errors.Is(stoppedDiscoveryResult(inProgress), inProgress) {
		t.Fatal("unfinished operation was hidden")
	}
	notReady := dbus.NewError("org.bluez.Error.NotReady", []any{"Resource Not Ready"})
	if !errors.Is(stoppedDiscoveryResult(notReady), notReady) {
		t.Fatal("adapter failure was hidden")
	}
}
