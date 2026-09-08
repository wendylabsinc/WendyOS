package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

// ── hostPort ────────────────────────────────────────────────────────

func TestHostPort(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		// IPv4
		{"IPv4", "192.168.1.5", 50051, "192.168.1.5:50051"},
		{"IPv4 loopback", "127.0.0.1", 50051, "127.0.0.1:50051"},
		{"IPv4 alt port", "10.0.0.1", 8080, "10.0.0.1:8080"},

		// IPv6 global — must be bracketed
		{"IPv6 global", "2001:db8::1", 50051, "[2001:db8::1]:50051"},
		{"IPv6 loopback", "::1", 50051, "[::1]:50051"},
		{"IPv6 full", "2001:0db8:85a3:0000:0000:8a2e:0370:7334", 50051, "[2001:0db8:85a3:0000:0000:8a2e:0370:7334]:50051"},

		// IPv6 link-local with zone ID — must be bracketed
		{"IPv6 zone en0", "fe80::3ee2:fcc9:fe8e:f69c%en0", 50051, "[fe80::3ee2:fcc9:fe8e:f69c%en0]:50051"},
		{"IPv6 zone en24 (USB)", "fe80::8c13:12bf:4df8:b976%en24", 50051, "[fe80::8c13:12bf:4df8:b976%en24]:50051"},
		{"IPv6 zone eth0 (Linux)", "fe80::1%eth0", 50051, "[fe80::1%eth0]:50051"},
		{"IPv6 zone numeric", "fe80::1%5", 50051, "[fe80::1%5]:50051"},
		{"IPv6 zone mTLS port", "fe80::1%en0", 50052, "[fe80::1%en0]:50052"},

		// Hostnames — no brackets
		{"mDNS hostname", "wendyos-otter.local", 50051, "wendyos-otter.local:50051"},
		{"plain hostname", "my-device", 50051, "my-device:50051"},
		{"FQDN", "device.example.com", 50051, "device.example.com:50051"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostPort(tt.host, tt.port)
			if got != tt.want {
				t.Fatalf("hostPort(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestResolveAgentPlatform(t *testing.T) {
	tests := []struct {
		name        string
		cfgPlatform string
		agentOS     string
		agentArch   string
		want        string
	}{
		{
			name:        "full platform is used as-is",
			cfgPlatform: "linux/amd64",
			agentOS:     "darwin",
			agentArch:   "arm64",
			want:        "linux/amd64",
		},
		{
			name:        "full wendyos platform is normalized to linux",
			cfgPlatform: "wendyos/arm64",
			agentOS:     "darwin",
			agentArch:   "amd64",
			want:        "linux/arm64",
		},
		{
			name:        "OS-only platform uses agent architecture",
			cfgPlatform: "darwin",
			agentOS:     "linux",
			agentArch:   "arm64",
			want:        "darwin/arm64",
		},
		{
			name:        "OS-only wendyos platform is normalized to linux",
			cfgPlatform: "wendyos",
			agentOS:     "darwin",
			agentArch:   "arm64",
			want:        "linux/arm64",
		},
		{
			name:      "empty platform defaults to linux on Linux agent",
			agentOS:   "linux",
			agentArch: "arm64",
			want:      "linux/arm64",
		},
		{
			name:      "empty platform defaults to linux on Darwin agent",
			agentOS:   "darwin",
			agentArch: "arm64",
			want:      "linux/arm64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAgentPlatform(tt.cfgPlatform, tt.agentOS, tt.agentArch)
			if got != tt.want {
				t.Fatalf("resolveAgentPlatform(%q, %q, %q) = %q, want %q", tt.cfgPlatform, tt.agentOS, tt.agentArch, got, tt.want)
			}
		})
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

func TestExternalProviderPickerHint(t *testing.T) {
	tests := []struct {
		name        string
		providerKey string
		want        string
	}{
		{
			name:        "docker",
			providerKey: providers.ProviderKeyDocker,
			want:        "Docker",
		},
		{
			name:        "local",
			providerKey: providers.ProviderKeyLocal,
			want:        providers.LocalDisplayName(),
		},
		{
			name:        "other",
			providerKey: "wendy-lite",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := externalProviderPickerHint(tt.providerKey)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("hint = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("hint = %q, want it to mention %q", got, tt.want)
			}
			for _, stale := range []string{"Docker Desktop", "Local Machine"} {
				if strings.Contains(got, stale) {
					t.Fatalf("hint = %q, want long label %q replaced", got, stale)
				}
			}
		})
	}
}

func TestProvisionedAgentAdvertisedMTLSMatchesDiscoveredMTLSDevice(t *testing.T) {
	stubDiscoverLANDevices(t, []models.LANDevice{
		{
			IPAddress: "127.0.0.1",
			Port:      defaultAgentPort + agentMTLSPortOffset,
			IsMTLS:    true,
		},
	}, nil)

	if !provisionedAgentAdvertisedMTLS(context.Background(), "127.0.0.1:50051") {
		t.Fatal("provisionedAgentAdvertisedMTLS() = false, want true")
	}
}

func TestProvisionedAgentAdvertisedMTLSIgnoresPlaintextDevices(t *testing.T) {
	stubDiscoverLANDevices(t, []models.LANDevice{
		{
			IPAddress: "127.0.0.1",
			Port:      defaultAgentPort,
			IsMTLS:    false,
		},
	}, nil)

	if provisionedAgentAdvertisedMTLS(context.Background(), "127.0.0.1:50051") {
		t.Fatal("provisionedAgentAdvertisedMTLS() = true, want false")
	}
}

func TestProvisionedAgentAdvertisedMTLSMatchesHostnameCaseInsensitively(t *testing.T) {
	stubDiscoverLANDevices(t, []models.LANDevice{
		{
			Hostname: "WENDYOS-OTTER.LOCAL.",
			Port:     defaultAgentPort + agentMTLSPortOffset,
			IsMTLS:   true,
		},
	}, nil)

	if !provisionedAgentAdvertisedMTLS(context.Background(), "wendyos-otter.local:50051") {
		t.Fatal("provisionedAgentAdvertisedMTLS() = false, want true")
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

func TestLANAgentAddresses_IPv6LinkLocal(t *testing.T) {
	dev := models.LANDevice{
		IPAddress: "fe80::8c13:12bf:4df8:b976%en24",
		Hostname:  "wendyos-otter.local",
		Port:      defaultAgentPort,
	}

	got := lanAgentAddresses(dev)
	want := []string{
		"[fe80::8c13:12bf:4df8:b976%en24]:50051",
		"wendyos-otter.local:50051",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanAgentAddresses() = %v, want %v", got, want)
	}
}

func TestLANAgentAddresses_IPv6OnlyNoHostname(t *testing.T) {
	dev := models.LANDevice{
		IPAddress: "fe80::1%en0",
		Port:      defaultAgentPort,
	}

	got := lanAgentAddresses(dev)
	want := []string{"[fe80::1%en0]:50051"}

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

func TestIsCertRejectionErrorIgnoresPlaintextTLSProbe(t *testing.T) {
	err := errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`)

	if isCertRejectionError("192.168.2.253:50052", err) {
		t.Fatal("isCertRejectionError() = true, want false for plaintext TLS probe")
	}
}

func TestIsCertRejectionErrorDetectsTLSAlert(t *testing.T) {
	err := errors.New("rpc error: code = Unavailable desc = remote error: tls: bad certificate")

	if !isCertRejectionError("192.168.2.253:50052", err) {
		t.Fatal("isCertRejectionError() = false, want true for TLS alert")
	}
}

func TestResolveLANAgentVersionFallsBackAcrossAddresses(t *testing.T) {
	orig := getAgentVersionAtAddress
	defer func() { getAgentVersionAtAddress = orig }()

	var (
		mu    sync.Mutex
		calls []string
	)
	getAgentVersionAtAddress = func(_ context.Context, address string) (bool, *agentpb.GetAgentVersionResponse, error) {
		mu.Lock()
		calls = append(calls, address)
		mu.Unlock()

		if address == "192.168.1.23:50051" {
			return false, nil, errors.New("dial tcp 192.168.1.23:50051: i/o timeout")
		}
		return false, &agentpb.GetAgentVersionResponse{Version: "1.2.3"}, nil
	}

	dev := models.LANDevice{
		IPAddress: "192.168.1.23",
		Hostname:  "wendyos-otter.local",
		Port:      defaultAgentPort,
	}

	address, _, resp, err := resolveLANAgentVersion(context.Background(), dev)
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

// TestResolveLANAgentVersionAllowsMTLSHandshakeTime guards against the
// nested-timeout inversion that left provisioned devices stuck on the failure
// glyph in the discover picker: the per-address probe budget
// (lanAddressProbeBudget) must comfortably contain a single autoTLS
// connect+probe, whose own budget is mtlsProbeTimeout. A provisioned device's
// handshake takes ~2.2s, so a per-address budget shorter than mtlsProbeTimeout
// cancelled the mTLS probe before it could answer — even though `wendy device
// info` (which connects with the un-capped root context) succeeded.
func TestResolveLANAgentVersionAllowsMTLSHandshakeTime(t *testing.T) {
	orig := getAgentVersionAtAddress
	defer func() { getAgentVersionAtAddress = orig }()

	// Simulate a provisioned device whose autoTLS handshake takes longer than
	// the old 1500ms budget but well within a single mtlsProbeTimeout.
	const handshake = 2200 * time.Millisecond
	getAgentVersionAtAddress = func(ctx context.Context, _ string) (bool, *agentpb.GetAgentVersionResponse, error) {
		select {
		case <-time.After(handshake):
			return true, &agentpb.GetAgentVersionResponse{Version: "9.9.9"}, nil
		case <-ctx.Done():
			return false, nil, ctx.Err()
		}
	}

	dev := models.LANDevice{IPAddress: "192.168.1.50", Port: defaultAgentPort}

	_, _, resp, err := resolveLANAgentVersion(context.Background(), dev)
	if err != nil {
		t.Fatalf("resolveLANAgentVersion() error = %v; per-address probe budget too short to complete an mTLS handshake", err)
	}
	if resp.GetVersion() != "9.9.9" {
		t.Fatalf("resolveLANAgentVersion() version = %q, want %q", resp.GetVersion(), "9.9.9")
	}
}

// TestMTLSBudgetInvariants guards the timeout-budget relationships explained
// in the comments on mtlsProbeTimeout/lanAddressProbeBudget, so a future edit
// can't silently invert or shrink them below what a slow post-quantum ML-DSA
// handshake on constrained hardware (Jetson, Raspberry Pi) needs. Regressing
// any of these was the direct cause of two prior flakes: provisioned LAN rows
// stuck on the failure glyph (PR #1297/#1309) and, most recently, direct
// `wendy device` commands intermittently reporting a spurious "Unauthorized"
// for a device that was actually up and holding a valid certificate.
func TestMTLSBudgetInvariants(t *testing.T) {
	const minTolerableHandshake = 6 * time.Second

	if mtlsProbeTimeout < minTolerableHandshake {
		t.Fatalf("mtlsProbeTimeout = %s, want >= %s to tolerate a slow ML-DSA handshake on constrained hardware", mtlsProbeTimeout, minTolerableHandshake)
	}
	// Evaluate the single-cert budget (the old lanAddressProbeTimeout) against
	// mtlsProbeTimeout — a single mTLS probe must never be cancelled before it
	// can answer.
	singleCertBudget := lanAddressProbeBudget(1)
	if singleCertBudget <= mtlsProbeTimeout {
		t.Fatalf("lanAddressProbeBudget(1) (%s) must be strictly greater than mtlsProbeTimeout (%s), or a single mTLS probe can be cancelled before it answers", singleCertBudget, mtlsProbeTimeout)
	}
	if headroom := singleCertBudget - mtlsProbeTimeout; headroom < time.Second {
		t.Fatalf("lanAddressProbeBudget(1) headroom over mtlsProbeTimeout = %s, want >= 1s so the two budgets can't converge to the point of flaking again", headroom)
	}

	// A truly-unreachable device must still fail in a bounded time, not
	// minutes. Note the total is NOT a fixed wall-clock number: a single
	// connectWithAutoTLSDiagnostics attempt probes 2 address candidates
	// (plaintextAddr and port+1) for *each* stored certificate and then makes
	// one agentPlaintextProbeTimeout-bounded plaintext probe, so the true worst
	// case scales with len(loadAllCLICerts()). retryOnHandshakeTimeout only
	// multiplies that by (maxHandshakeTimeoutRetries+1). Guard the two factors
	// this change actually controls: the retry multiplier stays small, and a
	// single-certificate attempt (the common case) stays well under a minute.
	if maxHandshakeTimeoutRetries > 3 {
		t.Fatalf("maxHandshakeTimeoutRetries = %d, want <= 3 so a genuinely-down device isn't retried into a multi-minute stall", maxHandshakeTimeoutRetries)
	}
	const maxSanePerCertBudget = 60 * time.Second
	singleCertAttempt := 2*mtlsProbeTimeout + agentPlaintextProbeTimeout
	worstCasePerCert := time.Duration(maxHandshakeTimeoutRetries+1) * singleCertAttempt
	if worstCasePerCert > maxSanePerCertBudget {
		t.Fatalf("worst-case per-certificate direct-connect budget = %s ((retries+1) * (2*mtlsProbeTimeout + agentPlaintextProbeTimeout)), want <= %s so a genuinely-down device with one stored cert fails in bounded time", worstCasePerCert, maxSanePerCertBudget)
	}
}

// TestLANAddressProbeBudgetScalesWithOrgCount pins the multi-org fix:
// connectWithAutoTLSDiagnostics tries every stored org cert in turn, so the
// per-address budget must grow with the number of orgs. A user logged into
// orgs [57, 2] whose device is in org 2 has its matching cert tried *second*;
// with the old fixed single-probe budget the org-2 probe was cancelled before
// it answered, so the picker showed a failure glyph even though `wendy device
// info` (uncapped) connected fine.
func TestLANAddressProbeBudgetScalesWithOrgCount(t *testing.T) {
	single := lanAddressProbeBudget(1)
	if want := mtlsProbeTimeout + 2*time.Second; single != want {
		t.Fatalf("lanAddressProbeBudget(1) = %v, want %v", single, want)
	}
	// Zero/negative cert counts clamp to the single-probe budget.
	if got := lanAddressProbeBudget(0); got != single {
		t.Fatalf("lanAddressProbeBudget(0) = %v, want %v (clamped)", got, single)
	}
	// Each additional org must add at least one mTLS probe of headroom, so the
	// last cert tried still has a full mtlsProbeTimeout to answer.
	if got := lanAddressProbeBudget(2); got < single+mtlsProbeTimeout {
		t.Fatalf("lanAddressProbeBudget(2) = %v, want >= %v", got, single+mtlsProbeTimeout)
	}
	// Budget must be strictly monotonic in the org count.
	if lanAddressProbeBudget(3) <= lanAddressProbeBudget(2) {
		t.Fatal("lanAddressProbeBudget must increase with org count")
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

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isDefault {
		t.Fatal("expected isDefault=false when --device flag is set")
	}
	if addr != "my-device.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "my-device.local:50051")
	}
	if pinKey != "my-device.local" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "my-device.local")
	}
}

func TestResolveDeviceAddress_DefaultDevice(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{DefaultDevice: "wendy-thor.local"})

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isDefault {
		t.Fatal("expected isDefault=true when using default device from config")
	}
	if addr != "wendy-thor.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "wendy-thor.local:50051")
	}
	if pinKey != "wendy-thor.local" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "wendy-thor.local")
	}
}

func TestResolveDeviceAddress_ExplicitHostPortFlag(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = "my-mac.local:50051"

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isDefault {
		t.Fatal("expected isDefault=false when --device flag is set")
	}
	if addr != "my-mac.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "my-mac.local:50051")
	}
	if pinKey != "my-mac.local" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "my-mac.local")
	}
}

func TestResolveDeviceAddress_ExplicitHostPortDefault(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{DefaultDevice: "my-mac.local:50051"})

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isDefault {
		t.Fatal("expected isDefault=true when using default device from config")
	}
	if addr != "my-mac.local:50051" {
		t.Fatalf("addr = %q, want %q", addr, "my-mac.local:50051")
	}
	if pinKey != "my-mac.local" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "my-mac.local")
	}
}

func TestResolveDeviceAddress_IPv6ZoneFlag(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = "fe80::8c13:12bf:4df8:b976%en24"

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isDefault {
		t.Fatal("expected isDefault=false when --device flag is set")
	}
	if addr != "[fe80::8c13:12bf:4df8:b976%en24]:50051" {
		t.Fatalf("addr = %q, want %q", addr, "[fe80::8c13:12bf:4df8:b976%en24]:50051")
	}
	if pinKey != "fe80::8c13:12bf:4df8:b976%en24" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "fe80::8c13:12bf:4df8:b976%en24")
	}
}

func TestResolveDeviceAddress_IPv6DefaultDevice(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{DefaultDevice: "fe80::1%en0"})

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isDefault {
		t.Fatal("expected isDefault=true when using default device from config")
	}
	if addr != "[fe80::1%en0]:50051" {
		t.Fatalf("addr = %q, want %q", addr, "[fe80::1%en0]:50051")
	}
	if pinKey != "fe80::1%en0" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "fe80::1%en0")
	}
}

func TestResolveDeviceAddress_IPv6GlobalFlag(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = "2001:db8::1"

	addr, pinKey, _, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "[2001:db8::1]:50051" {
		t.Fatalf("addr = %q, want %q", addr, "[2001:db8::1]:50051")
	}
	if pinKey != "2001:db8::1" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "2001:db8::1")
	}
}

func TestResolveDeviceAddress_IPv4Flag(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = "192.168.1.42"

	addr, pinKey, _, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr != "192.168.1.42:50051" {
		t.Fatalf("addr = %q, want %q", addr, "192.168.1.42:50051")
	}
	if pinKey != "192.168.1.42" {
		t.Fatalf("pinKey = %q, want %q — the pin must be filed under the name the user typed, not the dialled address", pinKey, "192.168.1.42")
	}
}

func TestResolveDeviceAddress_NoDevice(t *testing.T) {
	origFlag := deviceFlag
	defer func() { deviceFlag = origFlag }()
	deviceFlag = ""

	setTempConfig(t, &config.Config{})

	_, _, _, err := resolveDeviceAddress()
	if err == nil {
		t.Fatal("expected error when no device is specified")
	}
}

func TestDefaultDeviceSearchLabel(t *testing.T) {
	got := defaultDeviceSearchLabel("wendyos-daring-razorbill.local")
	want := `Searching for default device "wendyos-daring-razorbill.local"...`
	if got != want {
		t.Fatalf("defaultDeviceSearchLabel() = %q, want %q", got, want)
	}
}

func TestFormatElapsedSeconds(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{name: "fractional seconds", elapsed: 3420 * time.Millisecond, want: "3.42 seconds"},
		{name: "rounding", elapsed: 3449 * time.Millisecond, want: "3.45 seconds"},
		{name: "singular", elapsed: time.Second, want: "1.00 second"},
		{name: "rounds to singular", elapsed: 1004 * time.Millisecond, want: "1.00 second"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatElapsedSeconds(tt.elapsed); got != tt.want {
				t.Fatalf("formatElapsedSeconds(%v) = %q, want %q", tt.elapsed, got, tt.want)
			}
		})
	}
}

func TestConnectResolvedAgent_UsesSpinnerForInteractiveDefaultDevice(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origSpinner := runAgentConnectionSpinner
	origJSON := jsonOutput
	defer func() {
		isInteractiveTerminalFn = origInteractive
		runAgentConnectionSpinner = origSpinner
		jsonOutput = origJSON
	}()

	isInteractiveTerminalFn = func() bool { return true }
	jsonOutput = false

	wantConn := &grpcclient.AgentConnection{Host: "wendyos-daring-razorbill.local"}
	var (
		gotLabel       string
		spinnerInvoked bool
	)
	runAgentConnectionSpinner = func(_ context.Context, label string, _ func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
		spinnerInvoked = true
		gotLabel = label
		return wantConn, nil
	}

	gotConn, err := connectResolvedAgent(
		context.Background(),
		"wendyos-daring-razorbill.local",
		hostPort("wendyos-daring-razorbill.local", defaultAgentPort),
		true,
	)
	if err != nil {
		t.Fatalf("connectResolvedAgent() error = %v", err)
	}
	if !spinnerInvoked {
		t.Fatal("expected interactive default-device connection to use spinner")
	}
	if gotLabel != `Searching for default device "wendyos-daring-razorbill.local"...` {
		t.Fatalf("spinner label = %q, want %q", gotLabel, `Searching for default device "wendyos-daring-razorbill.local"...`)
	}
	if gotConn != wantConn {
		t.Fatal("connectResolvedAgent() did not return spinner result")
	}
}

func TestConnectResolvedAgent_NoAuthProvisionedAgentRequiresLogin(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origJSON := jsonOutput
	defer func() {
		isInteractiveTerminalFn = origInteractive
		jsonOutput = origJSON
	}()

	isInteractiveTerminalFn = func() bool { return false }
	jsonOutput = false
	setTempConfig(t, &config.Config{})

	plaintextAddr := startFailingPlaintextAgent(t)
	knownProvisionedMTLS := stubProvisionedMTLSDiscovery(t, plaintextAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := connectResolvedAgentWithProvisionedHint(ctx, "127.0.0.1", plaintextAddr, false, func() bool { return knownProvisionedMTLS })
	if conn != nil {
		conn.Close()
		t.Fatal("connectResolvedAgent() returned a connection for an auth-only agent")
	}
	if !errors.Is(err, errProvisionedAgentUnauthorized) {
		t.Fatalf("connectResolvedAgent() error = %v, want %v", err, errProvisionedAgentUnauthorized)
	}
	if err.Error() != provisionedAgentUnauthorizedMessage {
		t.Fatalf("connectResolvedAgent() message = %q, want %q", err.Error(), provisionedAgentUnauthorizedMessage)
	}
}

func TestConnectResolvedAgent_ProvisionedAgentPreservesMTLSError(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origJSON := jsonOutput
	defer func() {
		isInteractiveTerminalFn = origInteractive
		jsonOutput = origJSON
	}()

	isInteractiveTerminalFn = func() bool { return false }
	jsonOutput = false
	setTempConfig(t, &config.Config{
		Auth: []config.AuthConfig{
			{
				Certificates: []config.CertificateInfo{
					{
						PemCertificate: "not a certificate",
						PemPrivateKey:  "not a private key",
					},
				},
			},
		},
	})

	plaintextAddr := startFailingPlaintextAgent(t)
	knownProvisionedMTLS := stubProvisionedMTLSDiscovery(t, plaintextAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := connectResolvedAgentWithProvisionedHint(ctx, "127.0.0.1", plaintextAddr, false, func() bool { return knownProvisionedMTLS })
	if conn != nil {
		conn.Close()
		t.Fatal("connectResolvedAgent() returned a connection for an auth-only agent")
	}
	if !errors.Is(err, errProvisionedAgentUnauthorized) {
		t.Fatalf("connectResolvedAgent() error = %v, want %v", err, errProvisionedAgentUnauthorized)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("connectResolvedAgent() error = %v, want wrapped mTLS cause", err)
	}
	if !strings.Contains(err.Error(), "Last mTLS error:") || !strings.Contains(err.Error(), "loading TLS cert") {
		t.Fatalf("connectResolvedAgent() message = %q, want mTLS diagnostic", err.Error())
	}
}

func stubProvisionedMTLSDiscovery(t *testing.T, plaintextAddr string) bool {
	t.Helper()
	host, portStr, err := net.SplitHostPort(plaintextAddr)
	if err != nil {
		t.Fatalf("split plaintext address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse plaintext port: %v", err)
	}
	stubDiscoverLANDevices(t, []models.LANDevice{
		{
			IPAddress: host,
			Port:      port + agentMTLSPortOffset,
			IsMTLS:    true,
		},
	}, nil)
	return provisionedAgentAdvertisedMTLS(context.Background(), plaintextAddr)
}

func startFailingPlaintextAgent(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen plaintext candidate: %v", err)
	}
	go closeAcceptedConnections(listener)
	t.Cleanup(func() {
		listener.Close()
	})
	return listener.Addr().String()
}

func closeAcceptedConnections(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

func stubDiscoverLANDevices(t *testing.T, devices []models.LANDevice, err error) {
	t.Helper()

	orig := discoverLANDevices
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return devices, err
	}
	t.Cleanup(func() {
		discoverLANDevices = orig
	})
}

// ── device-cache fast path (connectWithAutoTLSDiagnostics) ────────────

// startPlaintextVersionAgent serves versionOnlyAgent (defined in
// helpers_socket_test.go) over plaintext TCP on loopback and returns its
// address ("127.0.0.1:PORT"). Used to simulate a real, reachable device for
// the device-cache fast-path tests below.
func startPlaintextVersionAgent(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, versionOnlyAgent{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// seedDeviceCache writes entries (each upserted, then flushed) to a fresh
// device-cache file at path, mirroring the discovery package's seedCache
// helper for the same discoverycache.Cache type.
func seedDeviceCache(t *testing.T, path string, entries ...discoverycache.Entry) {
	t.Helper()
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	now := time.Now()
	for _, e := range entries {
		c.Upsert(e, now)
	}
	if err := c.Flush(now); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// A fresh cache hit must dial the cached IP directly and never consult the OS
// resolver — that's the entire point of the fast path.
func TestConnectWithAutoTLSDiagnostics_CacheHitSkipsResolution(t *testing.T) {
	setTempConfig(t, &config.Config{}) // no certs → plaintext-only ladder

	origLoad, origLookup := deviceCacheLoadFn, osLookupHostFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
	}()

	realAddr := startPlaintextVersionAgent(t)
	realHost, realPort, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split real address: %v", err)
	}

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: realHost, Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	lookupCalls := 0
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		lookupCalls++
		return nil, errors.New("OS resolver should not run on a fresh cache hit")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:"+realPort)
	if err != nil {
		t.Fatalf("connectWithAutoTLSDiagnostics: %v", err)
	}
	defer conn.Close()
	if conn.Host != realHost {
		t.Fatalf("dialed host = %q, want cached IP %q", conn.Host, realHost)
	}
	if lookupCalls != 0 {
		t.Fatalf("osLookupHostFn called %d times, want 0 on a fresh cache hit", lookupCalls)
	}
}

// A cache miss must fall through to the pre-existing resolveAddrOnce path
// (OS resolver first), unchanged.
func TestConnectWithAutoTLSDiagnostics_CacheMissUsesOSResolver(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup := deviceCacheLoadFn, osLookupHostFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json") // never seeded: empty cache
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	realAddr := startPlaintextVersionAgent(t)
	realHost, realPort, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split real address: %v", err)
	}

	lookupCalls := 0
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		lookupCalls++
		return []string{realHost}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := connectWithAutoTLSDiagnostics(ctx, "orin2.local:"+realPort)
	if err != nil {
		t.Fatalf("connectWithAutoTLSDiagnostics: %v", err)
	}
	defer conn.Close()
	if lookupCalls == 0 {
		t.Fatal("expected osLookupHostFn to run on a cache miss")
	}
}

// A stale cached IP that fails to answer must never make an otherwise
// reachable device look unreachable (spec §4): the fast path must fall
// through to the same mDNS-browse fallback the cache-miss path uses. Here the
// TCP pre-check is the thing that fails, so the cached-IP ladder is skipped
// outright and the fall-through is the connect's first and only resolution —
// no second ladder pass is involved (the stale-retry-after-a-live-but-failing
// ladder path is covered separately by the LKG connect-flow tests).
// (The cache self-heal that a successful fall-through enables is a
// connectAgentAtAddressWithProvisionedHint-layer concern — see
// TestConnectAgentAtAddressWithProvisionedHint_SelfHealsExistingDiscoveryEntry
// — since connectWithAutoTLSDiagnostics's own "success" isn't proof of life
// for a plaintext connect and must never write the cache itself.)
func TestConnectWithAutoTLSDiagnostics_StaleCacheRetriesViaMDNS(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup, origBrowse, origTCP := deviceCacheLoadFn, osLookupHostFn, lanBrowseFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
		lanBrowseFn = origBrowse
		tcpDialTimeoutFn = origTCP
	}()

	realAddr := startPlaintextVersionAgent(t)
	realHost, realPort, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split real address: %v", err)
	}

	// 127.0.0.2 is loopback but nothing is bound there — a stale cached IP.
	const staleIP = "127.0.0.2"

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: staleIP, Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	// This entry is LKG-ineligible (MTLS unset), so the fromCache path now
	// runs through the same TCP-bounded pre-check (Finding 2) before it ever
	// reaches the ladder. Stub it dead — deterministic and instant, unlike a
	// real dial to an unbound loopback address, whose refusal timing is
	// environment-dependent — so the pre-check itself sends this straight to
	// fresh resolution, exactly the stale-IP path this test means to cover.
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}

	// The OS resolver can't see the device (the Windows/Linux ".local" gap
	// issue #1155 works around); only the mDNS browse fallback can — which is
	// what the post-pre-check fall-through must use to reach the device.
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{{Hostname: "orin.local", IPAddress: realHost}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:"+realPort)
	if err != nil {
		t.Fatalf("connectWithAutoTLSDiagnostics: %v", err)
	}
	defer conn.Close()
	if conn.Host != realHost {
		t.Fatalf("dialed host = %q, want re-resolved mDNS IP %q", conn.Host, realHost)
	}
}

// Regression: connectWithAutoTLSDiagnostics's own "success" is not proof of
// life for a plaintext connection (grpc.NewClient is lazy — see
// cacheFastPathReachable's doc), so it must never write to the device cache
// itself. Writing here unconditionally is exactly what previously caused
// polling loops like waitForAgentRestart/pollDeviceOnline (which call the
// lower-level connectWithAutoTLS/connectWithAutoTLSDiagnostics directly, not
// connectAgentAtAddressWithProvisionedHint) to phantom-refresh a DOWN
// device's cache entry on every iteration.
func TestConnectWithAutoTLSDiagnostics_DoesNotWriteCacheDirectly(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup := deviceCacheLoadFn, osLookupHostFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	realAddr := startPlaintextVersionAgent(t)
	realHost, realPort, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split real address: %v", err)
	}
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{realHost}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:"+realPort)
	if err != nil {
		t.Fatalf("connectWithAutoTLSDiagnostics: %v", err)
	}
	defer conn.Close()

	if _, err := os.Stat(cachePath); err == nil {
		t.Fatal("connectWithAutoTLSDiagnostics must not write the device cache itself")
	}
}

// A cert rejection against a CACHED address must be retried at the address the
// name resolves to now.
//
// This used to assert the opposite, on the reasoning that a completed handshake
// proves the cached address wasn't stale. On a network that rotates DHCP leases
// it isn't sound: the cached address gets reassigned to a different Wendy
// device, that device answers and legitimately fails the check, and skipping the
// re-resolve wedged the cache entry — reporting a correct pin as an identity
// problem, at an address the device had not held for hours. A handshake proves
// something answered, not that the right something answered at the right place.
func TestConnectWithAutoTLSDiagnostics_RejectionClassRetriedWhenAddressRotated(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup, origLadder, origReachable, origTCP := deviceCacheLoadFn, osLookupHostFn, dialAgentLadderFn, cacheFastPathReachableFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
		dialAgentLadderFn = origLadder
		cacheFastPathReachableFn = origReachable
		tcpDialTimeoutFn = origTCP
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	// This entry is LKG-ineligible (MTLS unset), so the fromCache path runs
	// through the same TCP-bounded pre-check. Stub it live so the test
	// exercises the fromCache ladder + retry logic, not a real network dial.
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}

	// The lease moved: the name now resolves somewhere else entirely.
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{"10.0.0.9"}, nil
	}

	var dialled []string
	rejectionErr := newTLSHandshakeRejectedError(errors.New("cert rejected"))
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = append(dialled, target.Addr)
		return nil, nil, rejectionErr
	}
	cacheFastPathReachableFn = func(context.Context, *grpcclient.AgentConnection, error) bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:50051")
	if !errors.Is(err, errTLSHandshakeRejected) {
		t.Fatalf("err = %v, want the handshake-rejected error to survive the retry", err)
	}
	if len(dialled) != 2 || dialled[0] != "10.0.0.5:50051" || dialled[1] != "10.0.0.9:50051" {
		t.Fatalf("addresses dialled = %v, want the cached address then the freshly resolved one", dialled)
	}
}

// A cross-org mismatch is the other rejection-class outcome and must equally be
// retried at the freshly resolved address — a reassigned lease is exactly how a
// device from another org ends up answering at the address we cached.
func TestConnectWithAutoTLSDiagnostics_OrgMismatchRetriedWhenAddressRotated(t *testing.T) {
	setTempConfig(t, &config.Config{})
	stubOrgNameResolver(t, nil)

	origLoad, origLookup, origLadder, origReachable, origTCP := deviceCacheLoadFn, osLookupHostFn, dialAgentLadderFn, cacheFastPathReachableFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
		dialAgentLadderFn = origLadder
		cacheFastPathReachableFn = origReachable
		tcpDialTimeoutFn = origTCP
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{"10.0.0.9"}, nil
	}

	calls := 0
	certs := []config.CertificateInfo{{OrganizationID: 3}}
	orgErr := chooseRejectionError(context.Background(), 42, certs, errors.New("boom"))
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		return nil, nil, orgErr
	}
	cacheFastPathReachableFn = func(context.Context, *grpcclient.AgentConnection, error) bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mismatch orgMismatchDeviceError
	_, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:50051")
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v (%T), want an orgMismatchDeviceError", err, err)
	}
	if calls != 2 {
		t.Fatalf("dialAgentLadderFn called %d times, want 2 (a rotated lease must be re-resolved before the mismatch is believed)", calls)
	}
}

// Re-resolving to the SAME address the cache already gave us is pure
// double-ladder latency with no chance of a different outcome — it must be
// skipped.
func TestConnectWithAutoTLSDiagnostics_SameAddressRetrySkipped(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup, origLadder, origReachable, origTCP := deviceCacheLoadFn, osLookupHostFn, dialAgentLadderFn, cacheFastPathReachableFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
		dialAgentLadderFn = origLadder
		cacheFastPathReachableFn = origReachable
		tcpDialTimeoutFn = origTCP
	}()

	const staleIP = "127.0.0.2"
	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: staleIP, Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	// This entry is LKG-ineligible (MTLS unset), so the fromCache path now
	// runs through the same TCP-bounded pre-check (Finding 2). Stub it live
	// so the test still exercises the fromCache ladder + same-address-skip
	// logic below, not a real network dial.
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}

	// Re-resolution via the OS resolver yields the EXACT same (stale) IP —
	// no new information for a retry to find.
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{staleIP}, nil
	}

	calls := 0
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		return nil, nil, errors.New("connection refused")
	}
	cacheFastPathReachableFn = func(context.Context, *grpcclient.AgentConnection, error) bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:50051"); err == nil {
		t.Fatal("expected the unresolved failure to surface")
	}
	if calls != 1 {
		t.Fatalf("dialAgentLadderFn called %d times, want 1 (re-resolving to the same address must not redial)", calls)
	}
}

// An already-expired context leaves no budget for a retry that can only fail
// the same way; it must be skipped rather than attempted.
func TestConnectWithAutoTLSDiagnostics_SkipsRetryWhenContextExpired(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLadder, origReachable, origTCP := deviceCacheLoadFn, dialAgentLadderFn, cacheFastPathReachableFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		dialAgentLadderFn = origLadder
		cacheFastPathReachableFn = origReachable
		tcpDialTimeoutFn = origTCP
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "127.0.0.2", Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	// This entry is LKG-ineligible (MTLS unset), so the fromCache path now
	// runs through the same TCP-bounded pre-check (Finding 2). Stub it live
	// so the test still exercises the expired-context retry-skip logic
	// below, not a real network dial.
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}

	calls := 0
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		return nil, nil, errors.New("connection refused")
	}
	cacheFastPathReachableFn = func(context.Context, *grpcclient.AgentConnection, error) bool { return false }

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if _, _, err := connectWithAutoTLSDiagnostics(ctx, "orin.local:50051"); err == nil {
		t.Fatal("expected an error when the cached IP fails under an already-expired context")
	}
	if calls != 1 {
		t.Fatalf("dialAgentLadderFn called %d times, want 1 (retry must be skipped once ctx is expired)", calls)
	}
}

// cacheFastPathReachable's own probe must be bounded by whatever's left of
// the caller's deadline, not the full agentPlaintextProbeTimeout — otherwise
// it can starve a subsequent retry (or the caller's own error handling) of
// the time budget the caller thought it had.
func TestCacheFastPathReachable_BoundsProbeByRemainingDeadline(t *testing.T) {
	realAddr := startPlaintextVersionAgent(t)
	conn, err := grpcclient.Connect(context.Background(), realAddr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	start := time.Now()
	reachable := cacheFastPathReachable(ctx, conn, nil)
	elapsed := time.Since(start)

	if reachable {
		t.Fatal("expected unreachable against an already-expired context")
	}
	if elapsed > time.Second {
		t.Fatalf("cacheFastPathReachable took %v against an already-expired context, want near-instant", elapsed)
	}
}

func TestIsMDNSShapedHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"orin", true},
		{"orin.local", true},
		{"Orin.LOCAL.", true},
		{"localhost", false},
		{"LocalHost", false},
		{"device.example.com", false}, // FQDN — never advertised over mDNS
		{"my-tunnel.wendy.example", false},
	}
	for _, c := range cases {
		if got := isMDNSShapedHost(c.host); got != c.want {
			t.Errorf("isMDNSShapedHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// Critical-path regression: a connect-success write must land under the SAME
// identity a discovery scan already established for this device (its TXT-
// id-derived ID/DisplayName), not mint a second row keyed by the hostname —
// end-to-end through connectAgentAtAddressWithProvisionedHint, the sole
// caller of cacheConnectSuccess.
func TestConnectAgentAtAddressWithProvisionedHint_SelfHealsExistingDiscoveryEntry(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup, origBrowse, origTCP := deviceCacheLoadFn, osLookupHostFn, lanBrowseFn, tcpDialTimeoutFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
		lanBrowseFn = origBrowse
		tcpDialTimeoutFn = origTCP
	}()

	realAddr := startPlaintextVersionAgent(t)
	realHost, realPort, err := net.SplitHostPort(realAddr)
	if err != nil {
		t.Fatalf("split real address: %v", err)
	}

	const staleIP = "127.0.0.2"
	const discoveryID = "3f9b2c10-91b4-4a52-9c11-000000000001"
	cachePath := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, cachePath, discoverycache.Entry{
		ID: discoveryID, DisplayName: "Orin Nano", Hostname: "orin.local",
		IP: staleIP, Port: 50051, AgentVersion: "1.4.0", OS: "wendyos",
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	// This entry is LKG-ineligible (MTLS unset), so the fromCache path now
	// runs through the same TCP-bounded pre-check (Finding 2) first. Stub it
	// dead — deterministic and instant — so the pre-check itself sends this
	// straight to fresh resolution via the stubs below.
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}

	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) {
		return []models.LANDevice{{Hostname: "orin.local", IPAddress: realHost}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := connectAgentAtAddressWithProvisionedHint(ctx, "orin.local:"+realPort, func() bool { return false })
	if err != nil {
		t.Fatalf("connectAgentAtAddressWithProvisionedHint: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.CachedAgentVersion(); !ok {
		t.Fatal("successful plaintext liveness probe was not retained on the connection")
	}

	reloaded, err := discoverycache.LoadFrom(cachePath)
	if err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("cache after connect = %+v, want exactly 1 entry (no duplicate key)", fresh)
	}
	e := fresh[0]
	if e.ID != discoveryID || e.DisplayName != "Orin Nano" {
		t.Errorf("identity = {ID:%q DisplayName:%q}, want the original discovery identity {ID:%q DisplayName:%q} preserved",
			e.ID, e.DisplayName, discoveryID, "Orin Nano")
	}
	if e.IP != realHost {
		t.Errorf("IP = %q, want self-healed to %q", e.IP, realHost)
	}
	if e.AgentVersion != "1.4.0" || e.OS != "wendyos" {
		t.Errorf("connect-only write wiped probed fields: AgentVersion=%q OS=%q", e.AgentVersion, e.OS)
	}
}

// Regression for the phantom-fresh bug: a connect whose plaintext dial is
// lazily "successful" but whose real post-connect probe fails (device
// actually down) must not touch the device cache at all.
func TestConnectAgentAtAddressWithProvisionedHint_FailedProbeDoesNotWriteCache(t *testing.T) {
	setTempConfig(t, &config.Config{})

	origLoad, origLookup := deviceCacheLoadFn, osLookupHostFn
	defer func() {
		deviceCacheLoadFn = origLoad
		osLookupHostFn = origLookup
	}()

	cachePath := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(cachePath) }

	failingAddr := startFailingPlaintextAgent(t)
	failingHost, failingPort, err := net.SplitHostPort(failingAddr)
	if err != nil {
		t.Fatalf("split failing address: %v", err)
	}
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		return []string{failingHost}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := connectAgentAtAddressWithProvisionedHint(ctx, "orin.local:"+failingPort, func() bool { return false }); err == nil {
		t.Fatal("expected the failed post-connect probe to surface as an error")
	}

	if _, err := os.Stat(cachePath); err == nil {
		t.Fatal("expected no cache file written after a failed post-connect probe")
	}
}

func TestCachedDeviceHostEntry(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	path := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, path, discoverycache.Entry{
		ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051,
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	if e, ok := cachedDeviceHostEntry("orin.local"); !ok || e.IP != "10.0.0.5" {
		t.Errorf("cachedDeviceHostEntry(fresh match) = %+v, %v; want IP %q, true", e, ok, "10.0.0.5")
	}
	if e, ok := cachedDeviceHostEntry("Orin.LOCAL."); !ok || e.IP != "10.0.0.5" {
		t.Errorf("cachedDeviceHostEntry(case/dot-insensitive) = %+v, %v; want IP %q, true", e, ok, "10.0.0.5")
	}
	if e, ok := cachedDeviceHostEntry("other.local"); ok {
		t.Errorf("cachedDeviceHostEntry(no match) = %+v, %v; want false", e, ok)
	}
}

func TestCachedDeviceHostEntry_MatchesStaleEntry(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	path := filepath.Join(t.TempDir(), "devices.json")
	c, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * discoverycache.TTL)
	c.Upsert(discoverycache.Entry{ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5"}, stale)
	if err := c.Flush(stale); err != nil {
		t.Fatal(err)
	}
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	if e, ok := cachedDeviceHostEntry("orin.local"); !ok || e.IP != "10.0.0.5" {
		t.Fatalf("cachedDeviceHostEntry(stale entry) = %+v, %v; want IP 10.0.0.5, true (connect lookup uses any-age entries)", e, ok)
	}
}

func TestCachedDeviceHostEntry_LoadErrorYieldsEmpty(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return nil, errors.New("boom") }

	if e, ok := cachedDeviceHostEntry("orin.local"); ok {
		t.Fatalf("cachedDeviceHostEntry(load error) = %+v, %v; want false", e, ok)
	}
}

func TestDialAgentLKGSkipsOnTCPPrecheckFailure(t *testing.T) {
	origTCP := tcpDialTimeoutFn
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		if timeout != lkgTCPConnectTimeout {
			t.Errorf("pre-check timeout = %v, want %v", timeout, lkgTCPConnectTimeout)
		}
		return nil, errors.New("no route to host")
	}
	ladderCalled := false
	origLadder := dialAgentLadderWithCertsFn
	dialAgentLadderWithCertsFn = func(ctx context.Context, target dialTarget, certs []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
		ladderCalled = true
		return nil, nil, errors.New("must not be reached")
	}
	t.Cleanup(func() { tcpDialTimeoutFn = origTCP; dialAgentLadderWithCertsFn = origLadder })

	_, _, outcome := dialAgentLKG(context.Background(), discoverycache.Entry{IP: "10.0.0.9", Port: 50052, MTLS: true, OrgID: 2}, "orin.local")
	if outcome != lkgDeadTCP {
		t.Fatalf("dialAgentLKG outcome = %v, want lkgDeadTCP", outcome)
	}
	if ladderCalled {
		t.Error("ladder dial ran after failed pre-check — dead IP must cost only the pre-check")
	}
}

func TestDialAgentLKGRotatesCertsAndDialsMTLSPort(t *testing.T) {
	origTCP := tcpDialTimeoutFn
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}
	var gotAddr, gotPinKey string
	var gotOrgs []int
	origLadder := dialAgentLadderWithCertsFn
	dialAgentLadderWithCertsFn = func(ctx context.Context, target dialTarget, certs []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
		gotAddr = target.Addr
		gotPinKey = target.PinKey
		for _, c := range certs {
			gotOrgs = append(gotOrgs, c.OrganizationID)
		}
		return &grpcclient.AgentConnection{IsMTLS: true}, nil, nil
	}
	origCerts := loadAllCLICertsFn
	loadAllCLICertsFn = func() []config.CertificateInfo {
		return []config.CertificateInfo{{OrganizationID: 1}, {OrganizationID: 2}}
	}
	t.Cleanup(func() {
		tcpDialTimeoutFn = origTCP
		dialAgentLadderWithCertsFn = origLadder
		loadAllCLICertsFn = origCerts
	})

	conn, _, outcome := dialAgentLKG(context.Background(), discoverycache.Entry{IP: "10.0.0.9", Port: 50052, MTLS: true, OrgID: 2}, "orin.local")
	if outcome != lkgConnected || conn == nil {
		t.Fatalf("dialAgentLKG outcome = %v, conn = %v; want lkgConnected with a connection", outcome, conn)
	}
	if gotAddr != "10.0.0.9:50052" {
		t.Errorf("dialed %q, want the entry's mTLS endpoint 10.0.0.9:50052", gotAddr)
	}
	if gotPinKey != "orin.local" {
		t.Errorf("pin key = %q, want the caller's requested name orin.local (never the cached IP)", gotPinKey)
	}
	if fmt.Sprint(gotOrgs) != fmt.Sprint([]int{2, 1}) {
		t.Errorf("cert org order = %v, want entry-org-first [2 1]", gotOrgs)
	}
}

func TestDialAgentLKGFallsThroughOnPlaintextDowngrade(t *testing.T) {
	origTCP := tcpDialTimeoutFn
	tcpDialTimeoutFn = func(network, addr string, timeout time.Duration) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go c2.Close()
		return c1, nil
	}
	origLadder := dialAgentLadderWithCertsFn
	dialAgentLadderWithCertsFn = func(ctx context.Context, target dialTarget, certs []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
		return grpcclient.NewFromConn(nil), nil, nil // IsMTLS=false: ladder fell to plaintext
	}
	origCerts := loadAllCLICertsFn
	loadAllCLICertsFn = func() []config.CertificateInfo { return []config.CertificateInfo{{OrganizationID: 1}} }
	t.Cleanup(func() {
		tcpDialTimeoutFn = origTCP
		dialAgentLadderWithCertsFn = origLadder
		loadAllCLICertsFn = origCerts
	})

	_, _, outcome := dialAgentLKG(context.Background(), discoverycache.Entry{IP: "10.0.0.9", Port: 50052, MTLS: true}, "orin.local")
	if outcome != lkgHandshakeFailed {
		t.Fatalf("LKG outcome = %v, want lkgHandshakeFailed for a plaintext downgrade of an entry advertised as mTLS", outcome)
	}
}

func TestCacheConnectSuccess_UpsertsFreshEntry(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	path := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	cacheConnectSuccess("orin.local:50051", &grpcclient.AgentConnection{Host: "10.0.0.9"})

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("got %d cached entries, want 1", len(fresh))
	}
	e := fresh[0]
	if e.IP != "10.0.0.9" {
		t.Errorf("IP = %q, want %q", e.IP, "10.0.0.9")
	}
	if e.Port != 50051 {
		t.Errorf("Port = %d, want 50051", e.Port)
	}
	if normalizeMDNSHost(e.Hostname) != "orin" {
		t.Errorf("Hostname = %q, does not normalize to %q", e.Hostname, "orin")
	}
	// No existing entry to reuse an identity from: mint one from the host.
	if e.ID != "orin" || e.DisplayName != "orin" {
		t.Errorf("ID/DisplayName = %q/%q, want host-derived %q/%q", e.ID, e.DisplayName, "orin", "orin")
	}
}

func TestCacheConnectSuccess_SkipsLiteralIPHost(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	path := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	cacheConnectSuccess("192.168.1.50:50051", &grpcclient.AgentConnection{Host: "192.168.1.50"})

	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected no cache file written for a literal-IP host")
	}
}

// Critical: when resolution fails entirely, dialAgentLadder's plaintext
// fallback can fall through to grpcclient.Connect with the raw, unresolved
// hostname, leaving conn.Host set to a NAME rather than an IP. Storing that
// in the IP field would poison the next cachedDeviceHostEntry lookup with
// exactly the ".local" resolution gap issue #1155 exists to work around.
func TestCacheConnectSuccess_RejectsNonIPDialedHost(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	path := filepath.Join(t.TempDir(), "devices.json")
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	cacheConnectSuccess("orin.local:50051", &grpcclient.AgentConnection{Host: "orin.local"})

	if _, err := os.Stat(path); err == nil {
		t.Fatal("expected no cache file written when the dialed host is a name, not an IP")
	}
}

// FQDNs/tunnel relays and "localhost" are never advertised over mDNS —
// minting a fabricated device-cache identity for them would be actively
// wrong, not merely useless.
func TestCacheConnectSuccess_SkipsNonMDNSShapedHost(t *testing.T) {
	for _, host := range []string{"localhost", "device.example.com"} {
		t.Run(host, func(t *testing.T) {
			orig := deviceCacheLoadFn
			defer func() { deviceCacheLoadFn = orig }()

			path := filepath.Join(t.TempDir(), "devices.json")
			deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

			cacheConnectSuccess(host+":50051", &grpcclient.AgentConnection{Host: "10.0.0.9"})

			if _, err := os.Stat(path); err == nil {
				t.Fatalf("expected no cache file written for non-mDNS-shaped host %q", host)
			}
		})
	}
}

// A cache-write failure (here: the load seam itself erroring) must never be
// allowed to surface — cacheConnectSuccess is best-effort and must not panic
// or otherwise disrupt an already-successful connect.
func TestCacheConnectSuccess_IgnoresLoadError(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return nil, errors.New("boom") }

	cacheConnectSuccess("orin.local:50051", &grpcclient.AgentConnection{Host: "10.0.0.9"})
}

// Critical fix: cacheConnectSuccess must write under an EXISTING entry (any age)
// identity (its discovery-assigned TXT-id ID/DisplayName) when one
// matches this hostname, not mint a second row under a host-derived key —
// otherwise the same physical device shows up twice and cachedDeviceHostEntry's
// next lookup can nondeterministically return a different row for the same
// device. Also covers Task 3's decision on record: this connect-only write
// (no probed AgentVersion/OS) must Upsert, not Replace, so it never wipes
// those fields.
func TestCacheConnectSuccess_ReusesExistingDiscoveryIdentity(t *testing.T) {
	orig := deviceCacheLoadFn
	defer func() { deviceCacheLoadFn = orig }()

	const discoveryID = "3f9b2c10-91b4-4a52-9c11-000000000001"
	path := filepath.Join(t.TempDir(), "devices.json")
	seedDeviceCache(t, path, discoverycache.Entry{
		ID: discoveryID, DisplayName: "Orin Nano", Hostname: "orin.local",
		IP: "10.0.0.5", Port: 50051, AgentVersion: "1.2.3", OS: "wendyos",
	})
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }

	cacheConnectSuccess("orin.local:50051", &grpcclient.AgentConnection{Host: "10.0.0.6"})

	reloaded, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reloaded.Fresh(time.Now())
	if len(fresh) != 1 {
		t.Fatalf("got %d entries, want exactly 1 (no duplicate key for the same device)", len(fresh))
	}
	e := fresh[0]
	if e.ID != discoveryID || e.DisplayName != "Orin Nano" {
		t.Errorf("identity = {ID:%q DisplayName:%q}, want the existing discovery identity {ID:%q DisplayName:%q} reused",
			e.ID, e.DisplayName, discoveryID, "Orin Nano")
	}
	if e.IP != "10.0.0.6" {
		t.Errorf("IP = %q, want refreshed %q", e.IP, "10.0.0.6")
	}
	if e.AgentVersion != "1.2.3" || e.OS != "wendyos" {
		t.Errorf("connect-only upsert wiped probed fields: AgentVersion=%q OS=%q", e.AgentVersion, e.OS)
	}
}

func TestCacheConnectSuccessStoresActualEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	seed, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	now := time.Now()
	// Discovery stored the advertised mTLS port; a connect via the plaintext
	// originalAddr port must NOT clobber it with 50051.
	seed.Upsert(discoverycache.Entry{ID: "dev-1", Hostname: "orin.local", IP: "10.0.0.9", Port: 50052, MTLS: true}, now)
	if err := seed.Flush(now); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	origLoad := deviceCacheLoadFn
	deviceCacheLoadFn = func() (*discoverycache.Cache, error) { return discoverycache.LoadFrom(path) }
	t.Cleanup(func() { deviceCacheLoadFn = origLoad })

	conn := &grpcclient.AgentConnection{Host: "10.0.0.9", IsMTLS: true, Addr: "10.0.0.9:50052"}
	cacheConnectSuccess("orin.local:50051", conn)

	after, _ := discoverycache.LoadFrom(path)
	e, ok := cachedDeviceEntry(after, "orin.local")
	if !ok {
		t.Fatal("entry missing after write-back")
	}
	if e.Port != 50052 {
		t.Errorf("Port = %d after mTLS connect on 50052, want 50052 (originalAddr's 50051 must not clobber)", e.Port)
	}
	if !e.MTLS {
		t.Error("MTLS flag lost on write-back")
	}
}

func TestProvisionedAgentUnauthorizedMentionsCLIUpgrade(t *testing.T) {
	// A reachability timeout against an mTLS-advertised device should hint at
	// both stale certs and a too-old CLI.
	err := newProvisionedAgentUnauthorizedError(errors.New("dial tcp 192.168.1.50:50051: i/o timeout"))
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "upgrade") || !strings.Contains(msg, "wendy auth refresh-certs") {
		t.Fatalf("message should mention upgrading the CLI and refresh-certs, got: %q", msg)
	}
}

func TestLanAgentAddressesPrefersUSBLinkLocal(t *testing.T) {
	tests := []struct {
		name string
		dev  models.LANDevice
		want []string
	}{
		{
			name: "usb present orders link-local before routed wifi ip",
			dev:  models.LANDevice{Hostname: "playful-reed.local", IPAddress: "192.168.1.50", USB: "en5 (USB Ethernet) 480 Mbps", Port: 50051},
			want: []string{"playful-reed.local:50051", "192.168.1.50:50051"},
		},
		{
			name: "no usb keeps ip-first ordering",
			dev:  models.LANDevice{Hostname: "playful-reed.local", IPAddress: "192.168.1.50", Port: 50051},
			want: []string{"192.168.1.50:50051", "playful-reed.local:50051"},
		},
		{
			name: "usb present but no ip falls back to hostname only",
			dev:  models.LANDevice{Hostname: "playful-reed.local", USB: "en5 (USB Ethernet) 480 Mbps", Port: 50051},
			want: []string{"playful-reed.local:50051"},
		},
		{
			// Probe-built device: the zoned link-local address is the verified
			// USB path, while .local is the name mDNS failed to serve.
			name: "zoned link-local from the usb probe outranks the .local name",
			dev:  models.LANDevice{Hostname: "playful-reed.local", IPAddress: "fe80::5741:1%enx0", USB: "enx0", Port: 50051},
			want: []string{"[fe80::5741:1%enx0]:50051", "playful-reed.local:50051"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lanAgentAddresses(tt.dev)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("lanAgentAddresses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCertRejectionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			// A plaintext (unprovisioned) agent probed with TLS: gRPC wraps the
			// "first record does not look like a TLS handshake" detail inside an
			// "authentication handshake failed" envelope. This is NOT a cert
			// rejection — the server simply isn't a TLS endpoint, so the CLI must
			// fall back to plaintext rather than report a bogus clock/cert error.
			"plaintext server probed with TLS",
			errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`),
			false,
		},
		{
			"server sent TLS alert (bad cert)",
			errors.New("rpc error: code = Unavailable desc = connection error: desc = \"remote error: tls: bad certificate\""),
			true,
		},
		{
			"client certificate required",
			errors.New("rpc error: code = Unavailable desc = connection error: desc = \"remote error: tls: certificate required\""),
			true,
		},
		{
			"plain transport error (connection refused)",
			errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:50052: connect: connection refused"`),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCertRejectionError("192.168.2.253:50052", tc.err); got != tc.want {
				t.Errorf("isCertRejectionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRotateCertsForOrg(t *testing.T) {
	certs := []config.CertificateInfo{
		{OrganizationID: 1}, {OrganizationID: 2}, {OrganizationID: 3}, {OrganizationID: 2},
	}
	orgs := func(cs []config.CertificateInfo) []int {
		out := make([]int, len(cs))
		for i, c := range cs {
			out[i] = c.OrganizationID
		}
		return out
	}
	cases := []struct {
		name  string
		orgID int32
		want  []int
	}{
		{"match moves first, stable within groups", 2, []int{2, 2, 1, 3}},
		{"zero org = unchanged", 0, []int{1, 2, 3, 2}},
		{"no match = unchanged", 9, []int{1, 2, 3, 2}},
	}
	for _, tc := range cases {
		got := orgs(rotateCertsForOrg(certs, tc.orgID))
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// Input slice must not be mutated.
	if fmt.Sprint(orgs(certs)) != fmt.Sprint([]int{1, 2, 3, 2}) {
		t.Error("rotateCertsForOrg mutated its input")
	}
}

func TestUpdateCheckTTLCache(t *testing.T) {
	tmp := t.TempDir()
	// Redirect os.UserCacheDir() on both darwin ($HOME/Library/Caches) and
	// linux ($XDG_CACHE_HOME or $HOME/.cache) into the temp dir.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))

	const host = "device.local"

	if updateCheckRecentlyPassed(host) {
		t.Fatal("cold: expected no recent pass before any check")
	}

	markUpdateCheckPassed(host)
	if !updateCheckRecentlyPassed(host) {
		t.Fatal("warm: expected recent pass after marking")
	}

	if updateCheckRecentlyPassed("other.local") {
		t.Fatal("marker must be per-host")
	}

	// Backdate the marker beyond the TTL: it must no longer count as recent.
	path := updateCheckMarkerPath(host)
	if path == "" {
		t.Fatal("expected a non-empty marker path")
	}
	old := time.Now().Add(-updateCheckTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if updateCheckRecentlyPassed(host) {
		t.Fatal("stale: expected marker older than TTL to fail the check")
	}
}

// TestHideLocalProviders verifies the device picker hides local run targets by
// default, preserves any caller-supplied excludes, leaves the input map
// untouched, and reveals local targets when WENDY_SHOW_LOCAL_DEVICES is set.
func TestHideLocalProviders(t *testing.T) {
	t.Run("hidden by default", func(t *testing.T) {
		t.Setenv(providers.ShowLocalDevicesEnv, "")
		got := hideLocalProviders(nil)
		for _, k := range providers.LocalProviderKeys() {
			if !got[k] {
				t.Errorf("hideLocalProviders(nil)[%q] = false; want true", k)
			}
		}
	})

	t.Run("preserves caller excludes and does not mutate input", func(t *testing.T) {
		t.Setenv(providers.ShowLocalDevicesEnv, "")
		in := map[string]bool{"wendy-lite": true}
		got := hideLocalProviders(in)
		if !got["wendy-lite"] {
			t.Error("hideLocalProviders dropped caller-supplied exclude wendy-lite")
		}
		if len(in) != 1 {
			t.Errorf("hideLocalProviders mutated input map: len = %d, want 1", len(in))
		}
	})

	t.Run("reveals local targets when opted in", func(t *testing.T) {
		t.Setenv(providers.ShowLocalDevicesEnv, "1")
		got := hideLocalProviders(nil)
		for _, k := range providers.LocalProviderKeys() {
			if got[k] {
				t.Errorf("hideLocalProviders(nil)[%q] = true with opt-in set; want false", k)
			}
		}
	})
}

// ── discoverProviderForPicker ───────────────────────────────────────

// fakeProvider is a minimal DeviceProvider for exercising the picker
// discovery loop.
type fakeProvider struct {
	key           string
	devices       []models.ExternalDevice
	discoverCalls atomic.Int32
}

func (p *fakeProvider) Key() string                             { return p.key }
func (p *fakeProvider) DisplayName() string                     { return "Fake" }
func (p *fakeProvider) IsAvailable(context.Context) bool        { return true }
func (p *fakeProvider) CheckRequirements(context.Context) error { return nil }
func (p *fakeProvider) DiscoverDevices(context.Context) ([]models.ExternalDevice, error) {
	p.discoverCalls.Add(1)
	return p.devices, nil
}
func (p *fakeProvider) SupportedBuildTypes() []string { return nil }
func (p *fakeProvider) CanBuild(string) bool          { return false }
func (p *fakeProvider) Build(context.Context, models.ExternalDevice, string, string, string, bool) (*providers.BuiltApp, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) Run(context.Context, *providers.BuiltApp, bool, chan<- providers.RunOutput) error {
	return errors.New("not implemented")
}
func (p *fakeProvider) Stop(context.Context, *providers.BuiltApp) error {
	return errors.New("not implemented")
}
func (p *fakeProvider) GetDeviceInfo(context.Context, models.ExternalDevice) (*providers.ProviderDeviceInfo, error) {
	return nil, nil
}

// fakeContinuousProvider additionally implements ContinuousDiscoverer.
type fakeContinuousProvider struct {
	fakeProvider
	ch  chan models.ExternalDevice
	err error
}

func (p *fakeContinuousProvider) DiscoverDevicesContinuous(context.Context) (<-chan models.ExternalDevice, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.ch, nil
}

var (
	_ providers.DeviceProvider       = (*fakeProvider)(nil)
	_ providers.ContinuousDiscoverer = (*fakeContinuousProvider)(nil)
)

// runDiscoverProviderForPicker starts the discovery loop in a goroutine and
// returns a channel of items sent to the picker plus a done channel closed
// when the loop returns.
func runDiscoverProviderForPicker(ctx context.Context, prov providers.DeviceProvider) (<-chan tui.PickerItem, <-chan struct{}) {
	got := make(chan tui.PickerItem, 16)
	done := make(chan struct{})
	go func() {
		discoverProviderForPicker(ctx, prov, func(items []tui.PickerItem) {
			for _, item := range items {
				got <- item
			}
		})
		close(done)
	}()
	return got, done
}

func awaitPickerItem(t *testing.T, got <-chan tui.PickerItem, wantName string) {
	t.Helper()
	select {
	case item := <-got:
		if item.Name != wantName {
			t.Fatalf("picker item name = %q, want %q", item.Name, wantName)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for picker item %q", wantName)
	}
}

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("discoverProviderForPicker did not return after ctx cancel")
	}
}

func TestProviderPollDelay(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"instant scan waits full cycle", 0, 3 * time.Second},
		{"scan time counts toward the cycle", 1 * time.Second, 2 * time.Second},
		{"remainder below minimum gap is clamped", 2600 * time.Millisecond, 500 * time.Millisecond},
		{"scan as long as the cycle", 3 * time.Second, 500 * time.Millisecond},
		{"scan longer than the cycle", 10 * time.Second, 500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerPollDelay(tt.elapsed); got != tt.want {
				t.Fatalf("providerPollDelay(%v) = %v, want %v", tt.elapsed, got, tt.want)
			}
		})
	}
}

func TestDiscoverProviderForPickerStreamsContinuous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov := &fakeContinuousProvider{
		fakeProvider: fakeProvider{key: "fake"},
		ch:           make(chan models.ExternalDevice),
	}
	got, done := runDiscoverProviderForPicker(ctx, prov)

	prov.ch <- models.ExternalDevice{ID: "fake:1", DisplayName: "one", ProviderKey: "fake"}
	awaitPickerItem(t, got, "one")
	prov.ch <- models.ExternalDevice{ID: "fake:2", DisplayName: "two", ProviderKey: "fake"}
	awaitPickerItem(t, got, "two")

	// Cancel then close the stream, as a real implementation would on ctx
	// cancellation; the loop must return without falling back to polling.
	cancel()
	close(prov.ch)
	awaitDone(t, done)

	if n := prov.discoverCalls.Load(); n != 0 {
		t.Fatalf("DiscoverDevices called %d times; streaming path should not poll", n)
	}
}

func TestDiscoverProviderForPickerPollingFallback(t *testing.T) {
	polled := []models.ExternalDevice{{ID: "fake:1", DisplayName: "polled", ProviderKey: "fake"}}

	tests := []struct {
		name string
		prov providers.DeviceProvider
	}{
		{"provider without continuous support", &fakeProvider{key: "fake", devices: polled}},
		{"continuous start error", &fakeContinuousProvider{
			fakeProvider: fakeProvider{key: "fake", devices: polled},
			err:          errors.New("stream unavailable"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			got, done := runDiscoverProviderForPicker(ctx, tt.prov)
			awaitPickerItem(t, got, "polled")
			cancel()
			awaitDone(t, done)
		})
	}
}

func TestDiscoverProviderForPickerStreamDeathFallsBackToPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov := &fakeContinuousProvider{
		fakeProvider: fakeProvider{
			key:     "fake",
			devices: []models.ExternalDevice{{ID: "fake:1", DisplayName: "polled", ProviderKey: "fake"}},
		},
		ch: make(chan models.ExternalDevice),
	}
	got, done := runDiscoverProviderForPicker(ctx, prov)

	// Stream dies while the picker is still open: polling must take over.
	close(prov.ch)
	awaitPickerItem(t, got, "polled")
	cancel()
	awaitDone(t, done)
}

func TestExternalProviderPickerItem(t *testing.T) {
	t.Run("wendy-lite devices become merged devices", func(t *testing.T) {
		prov := &fakeProvider{key: "wendy-lite"}
		dev := models.ExternalDevice{
			ID:              "wendy-lite:board",
			DisplayName:     "Lite Board",
			ProviderKey:     "wendy-lite",
			AgentVersion:    "1.2.3",
			CPUArchitecture: "riscv",
			ConnectionInfo:  map[string]string{"type": "LAN", "ip": "10.0.0.9"},
		}
		item := externalProviderPickerItem(prov, &dev)

		if item.Type != "LAN (Lite)" {
			t.Errorf("Type = %q, want %q", item.Type, "LAN (Lite)")
		}
		if item.Address != "10.0.0.9" {
			t.Errorf("Address = %q, want %q", item.Address, "10.0.0.9")
		}
		entry, ok := item.Value.(*pickerEntry)
		if !ok || entry.mergedDevice == nil {
			t.Fatalf("Value = %#v, want *pickerEntry with mergedDevice", item.Value)
		}
		if len(entry.mergedDevice.Externals) != 1 || entry.mergedDevice.Externals[0].ID != dev.ID {
			t.Errorf("mergedDevice.Externals = %#v, want the source device", entry.mergedDevice.Externals)
		}
		// A LAN row carries no serial port, so it must not take part in the
		// unflashed-row supersede at all.
		if item.DedupKey != dev.DisplayName || item.Supersedes != "" {
			t.Errorf("DedupKey = %q, Supersedes = %q, want the display name and no supersede",
				item.DedupKey, item.Supersedes)
		}
	})

	// An unflashed board and the same board once it identifies itself must share
	// one row: the unflashed row is keyed by port so the identified one can
	// retire it, instead of both display names sitting in the picker at once.
	t.Run("unflashed USB device is keyed by port", func(t *testing.T) {
		prov := &fakeProvider{key: "wendy-lite"}
		dev := models.ExternalDevice{
			ID:          "wendy-lite:/dev/cu.usbmodem2101",
			DisplayName: "ESP32 (unflashed) — /dev/cu.usbmodem2101",
			ProviderKey: "wendy-lite",
			ConnectionInfo: map[string]string{
				"type": "USB", "serialPort": "/dev/cu.usbmodem2101", "needsInstall": "true",
			},
		}
		item := externalProviderPickerItem(prov, &dev)

		if want := unflashedLiteDedupKey("/dev/cu.usbmodem2101"); item.DedupKey != want {
			t.Errorf("DedupKey = %q, want %q", item.DedupKey, want)
		}
		if item.Supersedes != "" {
			t.Errorf("Supersedes = %q, want empty on the unflashed row", item.Supersedes)
		}
		if want := strings.ToLower(dev.DisplayName); item.SortKey != want {
			t.Errorf("SortKey = %q, want %q so the row keeps its position", item.SortKey, want)
		}
	})

	t.Run("identified USB device supersedes the unflashed row", func(t *testing.T) {
		prov := &fakeProvider{key: "wendy-lite"}
		dev := models.ExternalDevice{
			ID:          "wendy-lite:/dev/cu.usbmodem2101",
			DisplayName: "Lite Board",
			ProviderKey: "wendy-lite",
			ConnectionInfo: map[string]string{
				"type": "USB", "serialPort": "/dev/cu.usbmodem2101", "name": "lite-board",
			},
		}
		item := externalProviderPickerItem(prov, &dev)

		if item.DedupKey != dev.DisplayName {
			t.Errorf("DedupKey = %q, want the display name so LAN/USB rows still merge", item.DedupKey)
		}
		if want := unflashedLiteDedupKey("/dev/cu.usbmodem2101"); item.Supersedes != want {
			t.Errorf("Supersedes = %q, want %q", item.Supersedes, want)
		}
	})

	// End to end over the picker: the reported bug was a board that stayed
	// listed as unflashed after a later probe identified it.
	t.Run("identified board replaces its unflashed row in the picker", func(t *testing.T) {
		prov := &fakeProvider{key: "wendy-lite"}
		const port = "/dev/cu.usbmodem2101"
		unflashed := models.ExternalDevice{
			ID:          "wendy-lite:" + port,
			DisplayName: "ESP32 (unflashed) — " + port,
			ProviderKey: "wendy-lite",
			ConnectionInfo: map[string]string{
				"type": "USB", "serialPort": port, "needsInstall": "true",
			},
		}
		identified := models.ExternalDevice{
			ID:          "wendy-lite:" + port,
			DisplayName: "Lite Board",
			ProviderKey: "wendy-lite",
			ConnectionInfo: map[string]string{
				"type": "USB", "serialPort": port, "name": "lite-board",
			},
		}

		picker := tui.NewPicker()
		updated, _ := picker.Update(tui.PickerAddMsg{
			Items: []tui.PickerItem{externalProviderPickerItem(prov, &unflashed)},
		})
		updated, _ = updated.(tui.PickerModel).Update(tui.PickerAddMsg{
			Items: []tui.PickerItem{externalProviderPickerItem(prov, &identified)},
		})

		view := updated.(tui.PickerModel).View()
		if !strings.Contains(view, identified.DisplayName) {
			t.Errorf("picker does not list the identified device:\n%s", view)
		}
		if strings.Contains(view, "unflashed") {
			t.Errorf("picker still lists the superseded unflashed row:\n%s", view)
		}
	})

	t.Run("other providers keep provider row layout", func(t *testing.T) {
		prov := &fakeProvider{key: "fake"}
		dev := models.ExternalDevice{ID: "fake:1", DisplayName: "Dev", ProviderKey: "fake"}
		item := externalProviderPickerItem(prov, &dev)

		if item.Type != "Fake" {
			t.Errorf("Type = %q, want %q", item.Type, "Fake")
		}
		entry, ok := item.Value.(*pickerEntry)
		if !ok || entry.externalDevice == nil {
			t.Fatalf("Value = %#v, want *pickerEntry with externalDevice", item.Value)
		}
		if entry.provider != providers.DeviceProvider(prov) {
			t.Errorf("entry.provider = %#v, want the source provider", entry.provider)
		}
	})
}

func TestCachedDeviceEntryAnyAgeMostRecentWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	cache, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	old := time.Now().Add(-3 * discoverycache.TTL)
	newer := time.Now().Add(-2 * discoverycache.TTL)
	// Two distinct device identities sharing one hostname (e.g. a device
	// re-provisioned under a new id): most recent LastSeen must win.
	cache.Upsert(discoverycache.Entry{ID: "dev-old", Hostname: "orin.local", IP: "10.0.0.8"}, old)
	cache.Upsert(discoverycache.Entry{ID: "dev-new", Hostname: "orin.local", IP: "10.0.0.9"}, newer)

	e, ok := cachedDeviceEntry(cache, "orin.local")
	if !ok {
		t.Fatal("stale entries not matched — connect lookup must be any-age")
	}
	if e.IP != "10.0.0.9" {
		t.Errorf("matched IP %q, want most-recent 10.0.0.9", e.IP)
	}
	// Bare-name form matches the .local stored form.
	if _, ok := cachedDeviceEntry(cache, "orin"); !ok {
		t.Error("bare hostname did not match .local entry")
	}
}

func TestIsCertRejectionErrorClassifiesHandshakeFailures(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{{
		name: "server sent a TLS alert",
		msg:  `rpc error: desc = "transport: authentication handshake failed: remote error: tls: bad certificate"`,
		want: true,
	}, {
		name: "server demanded a certificate",
		msg:  `rpc error: desc = "transport: authentication handshake failed: certificate required"`,
		want: true,
	}, {
		name: "plaintext agent probed with TLS",
		msg:  `rpc error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`,
		want: false,
	}, {
		// Off loopback an EOF may be an on-path reset, so it stays strict.
		name: "handshake ended in EOF",
		msg:  `rpc error: desc = "transport: authentication handshake failed: EOF"`,
		want: true,
	}}
	for _, tc := range cases {
		if got := isCertRejectionError("192.168.2.253:50052", errors.New(tc.msg)); got != tc.want {
			t.Errorf("%s: isCertRejectionError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoopbackEOFIsNotTreatedAsACertRejection(t *testing.T) {
	// QEMU's user-mode networking accepts on the host and only then finds the
	// guest port closed, so an unprovisioned VM's mTLS probe ends in EOF. That
	// must not suppress the plaintext rung -- but only for loopback, because
	// off it the same EOF may be an on-path reset.
	eof := errors.New(`rpc error: desc = "transport: authentication handshake failed: EOF"`)
	for _, addr := range []string{"127.0.0.1:50052", "localhost:50052", "[::1]:50052"} {
		if isCertRejectionError(addr, eof) {
			t.Errorf("%s: a loopback EOF still counts as a cert rejection", addr)
		}
	}
	if !isCertRejectionError("192.168.2.253:50052", eof) {
		t.Error("an EOF off loopback stopped counting as a cert rejection")
	}
}
