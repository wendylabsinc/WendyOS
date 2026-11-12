package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func stubNMCLIPath(t *testing.T, path string) {
	t.Helper()
	prev := nmcliPathFn
	nmcliPathFn = func() string { return path }
	t.Cleanup(func() { nmcliPathFn = prev })
}

func TestNetworkManagerUnavailableFromNMCLI(t *testing.T) {
	const list = "-t -f UUID,TYPE connection show"
	cases := []struct {
		name     string
		replies  []reply
		lastCall string
		want     bool
	}{
		{"not running on the first call", []reply{
			{args: list, out: "Error: NetworkManager is not running.", code: 8},
		}, list, true},
		{"stopped before the add", []reply{
			{args: list},
			{args: "connection add", prefix: true, out: "Error: NetworkManager is not running.", code: 8},
		}, "connection add", true},
		{"invalid passphrase", []reply{
			{args: list},
			{args: "connection add", prefix: true, out: "Error: 802-11-wireless-security.psk: property is invalid.", code: 2},
		}, "connection add", false},
		{"other failure", []reply{
			{args: list, out: "Error: Could not create NMClient object.", code: 1},
		}, list, false},
		{"no such connection", []reply{
			{args: list, code: nmcliNoSuchConnection},
		}, list, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, callLog := writeFakeNMCLI(t, c.replies...)
			stubNMCLIPath(t, path)
			err := AddOrUpdateProfile(context.Background(), SavedCredential{SSID: "Home", Password: "password1"})
			if err == nil {
				t.Fatal("AddOrUpdateProfile succeeded; want an error")
			}
			if calls := readCalls(t, callLog); !strings.HasPrefix(calls[len(calls)-1], c.lastCall) {
				t.Fatalf("last nmcli call %q; want it to start with %q", calls[len(calls)-1], c.lastCall)
			}
			if got := NetworkManagerUnavailable(err); got != c.want {
				t.Fatalf("NetworkManagerUnavailable(%v) = %v; want %v", err, got, c.want)
			}
		})
	}
}

func TestNetworkManagerUnavailableWithoutNMCLI(t *testing.T) {
	stubNMCLIPath(t, "")
	ctx := context.Background()
	for name, err := range map[string]error{
		"add":      AddOrUpdateProfile(ctx, SavedCredential{SSID: "Home"}),
		"activate": ActivateProfile(ctx, "Home"),
	} {
		if !errors.Is(err, ErrNMCLINotFound) || !NetworkManagerUnavailable(err) {
			t.Errorf("%s without nmcli = %v; want ErrNMCLINotFound, classified unavailable", name, err)
		}
	}
}

func TestNetworkManagerUnavailableSentinelOnly(t *testing.T) {
	if !NetworkManagerUnavailable(fmt.Errorf("registering: %w", ErrNMCLINotFound)) {
		t.Error("a wrapped ErrNMCLINotFound must be classified unavailable")
	}
	for _, err := range []error{nil, errors.New("nmcli not found"), errors.New("no saved profile")} {
		if NetworkManagerUnavailable(err) {
			t.Errorf("NetworkManagerUnavailable(%v) = true; want false", err)
		}
	}
}
