package commands

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestHostPort_IPv6LinkLocalWithZone(t *testing.T) {
	got := hostPort("fe80::3ee2:fcc9:fe8e:f69c%en0", 50051)
	want := "[fe80::3ee2:fcc9:fe8e:f69c%en0]:50051"
	if got != want {
		t.Fatalf("hostPort() = %q, want %q", got, want)
	}
}

func TestHostPort_IPv6Global(t *testing.T) {
	got := hostPort("2001:db8::1", 50051)
	want := "[2001:db8::1]:50051"
	if got != want {
		t.Fatalf("hostPort() = %q, want %q", got, want)
	}
}

func TestHostPort_IPv4(t *testing.T) {
	got := hostPort("192.168.1.5", 50051)
	want := "192.168.1.5:50051"
	if got != want {
		t.Fatalf("hostPort() = %q, want %q", got, want)
	}
}

func TestHostPort_Hostname(t *testing.T) {
	got := hostPort("wendyos-otter.local", 50051)
	want := "wendyos-otter.local:50051"
	if got != want {
		t.Fatalf("hostPort() = %q, want %q", got, want)
	}
}

func TestLANAgentAddressesPrefersIPAddress(t *testing.T) {
	dev := models.LANDevice{
		IPAddress: "192.168.1.23",
		Hostname:  "wendyos-otter.local",
		Port:      defaultAgentPort,
	}

	got := lanAgentAddresses(dev)
	want := []string{
		"192.168.1.23:50051",
		"wendyos-otter.local:50051",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanAgentAddresses() = %v, want %v", got, want)
	}
}

func TestLANAgentAddressesDeduplicatesIdenticalHosts(t *testing.T) {
	dev := models.LANDevice{
		IPAddress: "192.168.1.23",
		Hostname:  "192.168.1.23",
		Port:      defaultAgentPort,
	}

	got := lanAgentAddresses(dev)
	want := []string{"192.168.1.23:50051"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanAgentAddresses() = %v, want %v", got, want)
	}
}

func TestLANAgentAddressesFallsBackToDefaultPort(t *testing.T) {
	dev := models.LANDevice{
		IPAddress: "192.168.1.23",
		Hostname:  "wendyos-otter.local",
	}

	got := lanAgentAddresses(dev)
	want := []string{
		"192.168.1.23:50051",
		"wendyos-otter.local:50051",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanAgentAddresses() = %v, want %v", got, want)
	}
}

func TestResolveLANAgentVersionFallsBackAcrossAddresses(t *testing.T) {
	orig := getAgentVersionAtAddress
	defer func() { getAgentVersionAtAddress = orig }()

	var (
		mu    sync.Mutex
		calls []string
	)
	getAgentVersionAtAddress = func(_ context.Context, address string) (*agentpb.GetAgentVersionResponse, error) {
		mu.Lock()
		calls = append(calls, address)
		mu.Unlock()

		if address == "192.168.1.23:50051" {
			return nil, errors.New("dial tcp 192.168.1.23:50051: i/o timeout")
		}
		return &agentpb.GetAgentVersionResponse{Version: "1.2.3"}, nil
	}

	dev := models.LANDevice{
		IPAddress: "192.168.1.23",
		Hostname:  "wendyos-otter.local",
		Port:      defaultAgentPort,
	}

	address, resp, err := resolveLANAgentVersion(context.Background(), dev)
	if err != nil {
		t.Fatalf("resolveLANAgentVersion() error = %v", err)
	}

	if address != "wendyos-otter.local:50051" {
		t.Fatalf("resolveLANAgentVersion() address = %q, want %q", address, "wendyos-otter.local:50051")
	}
	if resp.GetVersion() != "1.2.3" {
		t.Fatalf("resolveLANAgentVersion() version = %q, want %q", resp.GetVersion(), "1.2.3")
	}

	wantCalls := []string{
		"192.168.1.23:50051",
		"wendyos-otter.local:50051",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("resolveLANAgentVersion() calls = %v, want %v", calls, wantCalls)
	}
}

// setTempConfig points HOME at a temp dir and writes cfg via config.Save so
// the test uses the same serialisation path as production code. t.Setenv
// automatically restores the original HOME when the test finishes.
func setTempConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// config.Save calls ConfigDir() which creates ~/.wendy and writes config.json.
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDeviceAddress_Flag(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = "my-device.local"

	addr, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isDefault {
		t.Fatal("expected isDefault=false when --device flag is set")
	}
	if addr != "my-device.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "my-device.local:50051")
	}
}

func TestResolveDeviceAddress_DefaultDevice(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{DefaultDevice: "wendy-thor.local"})

	addr, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isDefault {
		t.Fatal("expected isDefault=true when using default device from config")
	}
	if addr != "wendy-thor.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "wendy-thor.local:50051")
	}
}

func TestResolveDeviceAddress_NoDevice(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{})

	_, _, err := resolveDeviceAddress()
	if err == nil {
		t.Fatal("expected error when no device is specified")
	}
}

func TestResolveLANVersionsKeepsDevicesWhenMetadataLookupFails(t *testing.T) {
	orig := getAgentVersionAtAddress
	defer func() { getAgentVersionAtAddress = orig }()

	getAgentVersionAtAddress = func(_ context.Context, address string) (*agentpb.GetAgentVersionResponse, error) {
		return nil, errors.New("unreachable: " + address)
	}

	devices := []models.LANDevice{
		{
			DisplayName: "Wendy One",
			Hostname:    "wendy-one.local",
			IPAddress:   "192.168.1.10",
			Port:        defaultAgentPort,
		},
		{
			DisplayName: "Wendy Two",
			Hostname:    "wendy-two.local",
			IPAddress:   "192.168.1.11",
			Port:        defaultAgentPort,
		},
	}

	expected := make([]models.LANDevice, len(devices))
	copy(expected, devices)

	got := resolveLANVersions(context.Background(), devices)

	if len(got) != len(expected) {
		t.Fatalf("resolveLANVersions() returned %d devices, want %d", len(got), len(expected))
	}
	for i := range expected {
		if got[i].DisplayName != expected[i].DisplayName {
			t.Fatalf("resolveLANVersions()[%d].DisplayName = %q, want %q", i, got[i].DisplayName, expected[i].DisplayName)
		}
		if got[i].IPAddress != expected[i].IPAddress {
			t.Fatalf("resolveLANVersions()[%d].IPAddress = %q, want %q", i, got[i].IPAddress, expected[i].IPAddress)
		}
		if got[i].AgentVersion != "" {
			t.Fatalf("resolveLANVersions()[%d].AgentVersion = %q, want empty", i, got[i].AgentVersion)
		}
	}
}
