package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	scriptTenant    = "11111111-2222-4333-8444-555555555555"
	scriptPrincipal = "spiffe://wendy.sh/tenant/" + scriptTenant + "/device/lite-441bf6804824"
)

// scriptPairHandler's device sees a legacy source, a PKI-enrolled Wendy Lite
// board, a board needing enrollment, and one enrolled in a way this version
// cannot pair. The CLI is signed in to the legacy source's organization and
// the board's tenant.
func scriptPairHandler(client *pairTestClient) *sensorPairHandler {
	h := pairTestHandler(client)
	client.supportsPrincipal = true
	client.sources = append(client.sources,
		&agentpbv2.DiscoveredSensorSource{
			Id: "441bf6804824", DisplayName: "garage-cam", IpAddress: "10.42.0.17", Port: 5054, IsMtls: true,
			TenantUuid: scriptTenant, PrincipalUri: scriptPrincipal, Caps: []string{"sensors"}, WendyLite: true,
		},
		&agentpbv2.DiscoveredSensorSource{Id: "4824", DisplayName: "wendy-4824", IpAddress: "192.0.2.24", Port: 5054, Caps: []string{"sensors"}, WendyLite: true},
		&agentpbv2.DiscoveredSensorSource{Id: "lab", DisplayName: "lab-board", IpAddress: "192.0.2.25", Port: 5054, IsMtls: true, TenantUuid: scriptTenant, Caps: []string{"sensors"}, WendyLite: true},
	)
	h.tenantIDs = func() (map[string]bool, error) { return map[string]bool{scriptTenant: true}, nil }
	h.discoverLocal = func(context.Context) ([]models.DiscoveredDevice, error) {
		panic("a scripted pair must never scan this computer's network")
	}
	return h
}

// unnamedEnrollmentSource is a Wendy Lite board that needs enrollment and
// advertises no display name.
func unnamedEnrollmentSource() *agentpbv2.DiscoveredSensorSource {
	return &agentpbv2.DiscoveredSensorSource{Id: "4826", IpAddress: "192.0.2.26", Port: 5054, Caps: []string{"sensors"}, WendyLite: true}
}

func TestScriptedSensorPairMatchesByNameAssetIDOrPrincipal(t *testing.T) {
	for _, tc := range []struct {
		query     string
		asset     int32
		principal string
		transport string
	}{
		{"camera", 42, "", "grpc"},
		{" Camera ", 42, "", "grpc"},
		{"42", 42, "", "grpc"},
		{"GARAGE-CAM", 0, scriptPrincipal, "wendycom"},
		{scriptPrincipal, 0, scriptPrincipal, "wendycom"},
	} {
		client := &pairTestClient{}
		h := scriptPairHandler(client)
		h.name, h.sensors = "Front camera", []string{"video"}
		var out bytes.Buffer
		if err := runSensorPair(context.Background(), h, tc.query, true, &out); err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if client.adds != 1 || client.added.SourceAssetId != tc.asset || client.added.SourcePrincipalUri != tc.principal ||
			client.added.Transport != tc.transport || client.added.SourceAddress != "" ||
			client.added.Name != "Front camera" || strings.Join(client.added.SensorAllowlist, ",") != "video" {
			t.Fatalf("%q: adds=%d request=%+v", tc.query, client.adds, client.added)
		}
		var result struct {
			Pairing map[string]any `json:"pairing"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("%q: %v\n%s", tc.query, err, out.String())
		}
		if result.Pairing["name"] != "Front camera" || result.Pairing["source_principal_uri"] != tc.principal ||
			result.Pairing["source_asset_id"] != float64(tc.asset) || result.Pairing["connected"] != false {
			t.Fatalf("%q: pairing JSON %s", tc.query, out.String())
		}
	}
}

func TestScriptedSensorPairHumanOutput(t *testing.T) {
	client := &pairTestClient{}
	h := scriptPairHandler(client)
	client.sources[0].DisplayName = "Camera\x1b\r\u202e"
	var out bytes.Buffer
	if err := runSensorPair(context.Background(), h, "42", false, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Paired Camera. Its sensors will appear on this device.\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestScriptedSensorPairNotFoundListsSources(t *testing.T) {
	client := &pairTestClient{pairings: []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Offline sensor"}}}
	h := scriptPairHandler(client)
	client.sources = append(client.sources, unnamedEnrollmentSource())
	var out bytes.Buffer
	err := runSensorPair(context.Background(), h, "kitchen", true, &out)
	if err == nil || client.adds != 0 || out.Len() != 0 {
		t.Fatalf("err=%v adds=%d out=%q", err, client.adds, out.String())
	}
	for _, want := range []string{
		`no SensorLink source matches "kitchen". The device found:`,
		`"Camera" (asset 42, 192.0.2.42)`,
		`"garage-cam" (` + scriptPrincipal + `, 10.42.0.17)`,
		`"Offline sensor" (asset 99, paired, not found on the network)`,
		`"wendy-4824" (192.0.2.24, needs enrollment)`,
		`"lab-board" (192.0.2.25, enrolled, cannot be paired by this version)`,
		// Without a display name, a source is named by its address.
		`"192.0.2.26" (needs enrollment)`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}

	client = &pairTestClient{}
	h = pairTestHandler(client)
	client.sources = nil
	err = runSensorPair(context.Background(), h, "kitchen", true, &out)
	if err == nil || !strings.Contains(err.Error(), "the device found none on its network") || client.adds != 0 {
		t.Fatalf("empty network: %v", err)
	}
}

func TestScriptedSensorPairAmbiguousPairsNothing(t *testing.T) {
	client := &pairTestClient{}
	h := scriptPairHandler(client)
	client.sources = append(client.sources, &agentpbv2.DiscoveredSensorSource{AssetId: 43, OrgId: 7, DisplayName: "camera", IsMtls: true, Caps: []string{"sensors"}, IpAddress: "192.0.2.43"})
	// A source can also be named like another's asset ID.
	client.sources = append(client.sources, &agentpbv2.DiscoveredSensorSource{AssetId: 44, OrgId: 7, DisplayName: "42", IsMtls: true, Caps: []string{"sensors"}, IpAddress: "192.0.2.44"})
	err := runSensorPair(context.Background(), h, "Camera", true, &bytes.Buffer{})
	if err == nil || client.adds != 0 {
		t.Fatalf("err=%v adds=%d", err, client.adds)
	}
	for _, w := range []string{`"Camera" matches 2 SensorLink sources, so none was paired`, `"Camera" (asset 42`, `"camera" (asset 43`} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q in:\n%v", w, err)
		}
	}
	// The exact identity the error lists selects one, even when another
	// source is named like it: asset 42 is not the source named "42".
	for _, asset := range []int32{42, 43} {
		client.adds = 0
		query := strconv.Itoa(int(asset))
		if err := runSensorPair(context.Background(), h, query, true, &bytes.Buffer{}); err != nil || client.adds != 1 || client.added.SourceAssetId != asset {
			t.Fatalf("%q did not resolve the ambiguity: %v %+v", query, err, client.added)
		}
	}
}

func TestScriptedSensorPairRefusesSourcesItCannotPair(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		setup       func(*pairTestClient, *sensorPairHandler)
		want        string
		step        string
	}{
		{name: "needs enrollment", query: "wendy-4824", want: "wendy-4824 (192.0.2.24) needs enrollment before it can be paired", step: "Enroll it in the same organization"},
		{name: "enrolled but unsupported", query: "lab-board", want: "lab-board (192.0.2.25) is enrolled, but this version cannot pair it yet"},
		{
			name: "needs enrollment, unnamed", query: "192.0.2.26",
			setup: func(c *pairTestClient, _ *sensorPairHandler) {
				c.sources = append(c.sources, unnamedEnrollmentSource())
			},
			want: "192.0.2.26 needs enrollment before it can be paired", step: "Enroll it in the same organization",
		},
		{
			name: "already paired", query: "Camera",
			setup: func(c *pairTestClient, _ *sensorPairHandler) {
				c.pairings = []*agentpbv2.SensorPairing{{SourceAssetId: 42, Name: "Camera"}}
			},
			want: "Already paired with Camera.", step: "--forget",
		},
		{
			name: "saved pairing offline", query: "99",
			setup: func(c *pairTestClient, _ *sensorPairHandler) {
				c.pairings = []*agentpbv2.SensorPairing{{SourceAssetId: 99, Name: "Offline sensor"}}
			},
			want: "Already paired with Offline sensor.",
		},
		{
			name: "agent without principal pairing", query: "garage-cam",
			setup: func(c *pairTestClient, _ *sensorPairHandler) { c.supportsPrincipal = false },
			want:  "Update the target Wendy Agent to pair PKI-enrolled sensors.",
		},
		{
			name: "other organization", query: "Camera",
			setup: func(_ *pairTestClient, h *sensorPairHandler) {
				h.orgIDs = func() (map[int32]bool, error) { return map[int32]bool{8: true}, nil }
			},
			want: "device is in organization",
		},
		{
			name: "other tenant", query: "garage-cam",
			setup: func(_ *pairTestClient, h *sensorPairHandler) {
				h.tenantIDs = func() (map[string]bool, error) { return map[string]bool{}, nil }
			},
			want: "sign in to the sensor's organization before pairing",
		},
		{
			name: "discovery failed", query: "Camera",
			setup: func(c *pairTestClient, _ *sensorPairHandler) {
				c.discoverErr = status.Error(codes.Internal, "sensor source discovery failed on this device")
			},
			want: "SensorLink discovery failed: the device could not scan its network",
		},
		{
			name: "pairings unknown", query: "Camera",
			setup: func(c *pairTestClient, _ *sensorPairHandler) {
				c.err = status.Error(codes.Unavailable, "tunnel closed")
			},
			want: "could not load SensorLink pairings: device is not reachable",
		},
	} {
		client := &pairTestClient{}
		h := scriptPairHandler(client)
		if tc.setup != nil {
			tc.setup(client, h)
		}
		var out bytes.Buffer
		err := runSensorPair(context.Background(), h, tc.query, true, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || client.adds != 0 || out.Len() != 0 {
			t.Errorf("%s: err=%v adds=%d out=%q", tc.name, err, client.adds, out.String())
			continue
		}
		if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, tc.step) {
			t.Errorf("%s: next steps %q lack %q", tc.name, steps, tc.step)
		}
	}
}

// An agent that predates DiscoverSensorSources cannot scan from the device.
// The tab falls back to this computer's network; a script must not.
func TestScriptedSensorPairNeedsTheDeviceToScan(t *testing.T) {
	client := &pairTestClient{}
	h := scriptPairHandler(client)
	old := &unimplementedDiscoveryClient{client}
	h.client = old
	err := runSensorPair(context.Background(), h, "Camera", true, &bytes.Buffer{})
	if !errors.Is(err, errSensorDiscoveryUnsupported) || old.discoveries != 1 || client.adds != 0 {
		t.Fatalf("err=%v discoveries=%d adds=%d", err, old.discoveries, client.adds)
	}
	if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, "wendy device update") {
		t.Fatalf("next steps: %q", steps)
	}
	if h.discoverLocal == nil {
		t.Fatal("runSensorPair changed the caller's handler")
	}
}

func TestScriptedSensorListJSONAndText(t *testing.T) {
	client := &pairTestClient{supportsPrincipal: true, pairings: []*agentpbv2.SensorPairing{
		{SourceAssetId: 42, OrgId: 7, Name: "Front camera", SensorAllowlist: []string{"video"}, Connected: true, Transport: "grpc"},
		{SourcePrincipalUri: scriptPrincipal, Name: "garage\x1b\r\u202e", Transport: "wendycom"},
	}}
	var out bytes.Buffer
	if err := runSensorList(context.Background(), client, true, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Pairings                 []map[string]any `json:"pairings"`
		SupportsPrincipalPairing *bool            `json:"supports_principal_pairing"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Pairings) != 2 || result.SupportsPrincipalPairing == nil || !*result.SupportsPrincipalPairing {
		t.Fatalf("list JSON: %s", out.String())
	}
	first := result.Pairings[0]
	if first["source_asset_id"] != float64(42) || first["org_id"] != float64(7) || first["name"] != "Front camera" ||
		first["connected"] != true || first["transport"] != "grpc" || first["source_principal_uri"] != "" {
		t.Fatalf("pairing JSON: %s", out.String())
	}
	if !strings.HasPrefix(out.String(), "{\n  \"pairings\": [\n    {\n      \"source_asset_id\": 42,") {
		t.Fatalf("JSON layout: %q", out.String())
	}

	out.Reset()
	if err := runSensorList(context.Background(), client, false, &out); err != nil {
		t.Fatal(err)
	}
	// The long-standing layout: a 20-column name, the source, and its state.
	want := "Front camera" + strings.Repeat(" ", 9) + "Asset 42  connected\n" +
		"garage" + strings.Repeat(" ", 15) + "lite-441bf6804824  disconnected\n"
	if out.String() != want {
		t.Fatalf("text list = %q, want %q", out.String(), want)
	}

	client.pairings = nil
	out.Reset()
	if err := runSensorList(context.Background(), client, true, &out); err != nil || !strings.Contains(out.String(), `"pairings": []`) {
		t.Fatalf("empty list must be []: %s %v", out.String(), err)
	}

	client.err = status.Error(codes.Unavailable, "internal details")
	out.Reset()
	if err := runSensorList(context.Background(), client, true, &out); err == nil || !strings.Contains(err.Error(), "device is not reachable") || out.Len() != 0 {
		t.Fatalf("failed list: %v %q", err, out.String())
	}
}

func TestScriptedSensorForget(t *testing.T) {
	saved := func() []*agentpbv2.SensorPairing {
		return []*agentpbv2.SensorPairing{
			{SourceAssetId: 42, OrgId: 7, Name: "Front camera", Transport: "grpc", Connected: true},
			{SourcePrincipalUri: scriptPrincipal, Name: "garage", Transport: "wendycom"},
		}
	}
	for _, tc := range []struct {
		query     string
		asset     int32
		principal string
	}{
		{"front CAMERA", 42, ""},
		{"42", 42, ""},
		{scriptPrincipal, 0, scriptPrincipal},
	} {
		client := &pairTestClient{pairings: saved()}
		h := pairTestHandler(client)
		var out bytes.Buffer
		if err := runSensorForget(context.Background(), h, tc.query, true, &out); err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		// Forgetting works offline: it never scans.
		if client.removes != 1 || client.removed != tc.asset || client.removedPrincipal != tc.principal || client.discoveries != 0 {
			t.Fatalf("%q: removes=%d asset=%d principal=%q discoveries=%d", tc.query, client.removes, client.removed, client.removedPrincipal, client.discoveries)
		}
		var result struct {
			Forgotten map[string]any `json:"forgotten"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		// Asset 42 was connected when listed; once removed it has no link.
		if result.Forgotten["source_asset_id"] != float64(tc.asset) || result.Forgotten["source_principal_uri"] != tc.principal ||
			result.Forgotten["sensor_allowlist"] == nil || result.Forgotten["connected"] != false {
			t.Fatalf("%q: forget JSON %s", tc.query, out.String())
		}
		if tc.asset == 42 && !client.pairings[0].Connected {
			t.Fatalf("%q: forget changed the listed pairing", tc.query)
		}
	}

	client := &pairTestClient{pairings: saved()}
	h := pairTestHandler(client)
	var out bytes.Buffer
	if err := runSensorForget(context.Background(), h, "garage", false, &out); err != nil || out.String() != "Forgot garage.\n" {
		t.Fatalf("text forget: %q %v", out.String(), err)
	}

	client = &pairTestClient{pairings: saved()}
	h = pairTestHandler(client)
	err := runSensorForget(context.Background(), h, "kitchen", true, &out)
	if err == nil || client.removes != 0 || !strings.Contains(err.Error(), `no saved SensorLink pairing matches "kitchen". Saved pairings:`) ||
		!strings.Contains(err.Error(), `"Front camera" (asset 42, connected)`) || !strings.Contains(err.Error(), `"garage" (`+scriptPrincipal+`, disconnected)`) {
		t.Fatalf("not found: %v", err)
	}

	client.pairings = append(client.pairings, &agentpbv2.SensorPairing{SourceAssetId: 43, Name: "Garage"})
	err = runSensorForget(context.Background(), h, "garage", true, &out)
	if err == nil || client.removes != 0 || !strings.Contains(err.Error(), `"garage" matches 2 saved SensorLink pairings, so none was forgotten`) {
		t.Fatalf("ambiguous: %v", err)
	}

	// An asset ID forgets its own pairing, not one named like it.
	client.pairings = append(client.pairings, &agentpbv2.SensorPairing{SourceAssetId: 44, Name: "42"})
	if err := runSensorForget(context.Background(), h, "42", true, &out); err != nil || client.removes != 1 || client.removed != 42 {
		t.Fatalf("asset ID beside a pairing named like it: %v removes=%d asset=%d", err, client.removes, client.removed)
	}
	client.removes = 0

	client.pairings = nil
	if err := runSensorForget(context.Background(), h, "garage", true, &out); err == nil || !strings.Contains(err.Error(), "the device has none") {
		t.Fatalf("no pairings: %v", err)
	}

	client.pairings, client.err = saved(), status.Error(codes.PermissionDenied, "org mismatch")
	if err := runSensorForget(context.Background(), h, "garage", true, &out); err == nil || !strings.Contains(err.Error(), "not authorized") || client.removes != 0 {
		t.Fatalf("failed list: %v", err)
	}
}

func TestSensorPairFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		pair, forget                              bool
		source                                    string
		list, arms, unitree, options, wantAccepts bool
	}{
		{name: "tabs", wantAccepts: true},
		{name: "pair", pair: true, source: "cam", options: true, wantAccepts: true},
		{name: "forget", forget: true, source: "cam", wantAccepts: true},
		{name: "legacy list with options", list: true, options: true, wantAccepts: true},
		{name: "pair with usb arm flags", pair: true, source: "cam", arms: true},
		{name: "pair with unitree", pair: true, source: "cam", unitree: true},
		{name: "forget with unitree", forget: true, source: "cam", unitree: true},
		{name: "pair and list", pair: true, source: "cam", list: true},
		{name: "pair and forget", pair: true, forget: true, source: "cam"},
		{name: "forget and list", forget: true, source: "cam", list: true},
		{name: "empty source", pair: true, source: "  "},
		{name: "empty forget", forget: true},
		{name: "forget with options", forget: true, source: "cam", options: true},
	} {
		err := validateSensorPairFlags(tc.pair, tc.forget, tc.source, tc.list, tc.arms, tc.unitree, tc.options)
		if (err == nil) != tc.wantAccepts {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if err != nil && !IsUsageError(err) {
			t.Errorf("%s: %v is not a usage error", tc.name, err)
		}
	}
}

// Invalid combinations fail before connecting, and without a terminal the
// command asks for a scripted operation instead of opening the tabs.
func TestDevicePairCommandRejectsBeforeConnecting(t *testing.T) {
	previousInteractive, previousJSON := isInteractiveTerminalFn, jsonOutput
	t.Cleanup(func() { isInteractiveTerminalFn, jsonOutput = previousInteractive, previousJSON })
	isInteractiveTerminalFn = func() bool { return false }
	jsonOutput = false
	for _, flags := range [][]string{
		{"--sensorlink", "cam", "--usb-arms"},
		{"--sensorlink", "cam", "--unitree"},
		{"--sensorlink", "cam", "--ip", "192.168.1.42"},
		{"--sensorlink", "cam", "--list"},
		{"--sensorlink", "cam", "--forget", "cam"},
		{"--sensorlink", ""},
		{"--forget", "cam", "--name", "Front"},
		{"--forget", "cam", "--sensors", "video"},
		{"--forget", "cam", "--unitree"},
		{"--forget", "cam", "--list"},
		{"--sensorlink", "cam", "--bind", "usb-1"},
		{"--sensorlink", "cam", "--arm-name", "Left"},
		{"--sensorlink", "cam", "--allow-location-binding"},
		{"--sensorlink", "cam", "--usb-arms", "--name", "Front"},
		{},
		{"--name", "Front"},
		{"--unitree"},
	} {
		cmd := newDevicePairCmd()
		if err := cmd.ParseFlags(flags); err != nil {
			t.Fatal(err)
		}
		// connectToAgent never returns a usage error, so each of these
		// failed validation rather than the connection.
		if err := cmd.RunE(cmd, nil); !IsUsageError(err) {
			t.Fatalf("%v: want a usage error, got %v", flags, err)
		}
	}
	// The no-terminal refusal names the scripted operations.
	cmd := newDevicePairCmd()
	err := cmd.RunE(cmd, nil)
	if !IsUsageError(err) || !strings.Contains(err.Error(), "--sensorlink <source>") {
		t.Fatalf("tabs without a terminal: %v", err)
	}
	// --json asks for output the tabs cannot give, even in a terminal.
	isInteractiveTerminalFn, jsonOutput = func() bool { return true }, true
	if err := cmd.RunE(cmd, nil); !IsUsageError(err) {
		t.Fatalf("tabs with --json: %v", err)
	}
	// With --usb-arms, --forget keeps its USB meaning.
	if err := validateArmPairFlags(true, false, "", "usb-1", "", false, false, true); err != nil {
		t.Fatal(err)
	}
	if err := validateSensorPairFlags(false, false, "", false, true, false, false); err != nil {
		t.Fatal(err)
	}
}
