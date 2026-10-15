package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type armTestClient struct {
	pb.WendyRobotArmServiceClient
	devices   []*pb.RobotArm
	bound     *pb.BindRobotArmRequest
	forgotten string
	scans     int
	err       error
}

func (c *armTestClient) ListRobotArms(context.Context, *pb.ListRobotArmsRequest, ...grpc.CallOption) (*pb.ListRobotArmsResponse, error) {
	c.scans++
	return &pb.ListRobotArmsResponse{Devices: c.devices}, c.err
}
func (c *armTestClient) BindRobotArm(_ context.Context, r *pb.BindRobotArmRequest, _ ...grpc.CallOption) (*pb.BindRobotArmResponse, error) {
	c.bound = r
	return &pb.BindRobotArmResponse{Device: &pb.RobotArm{Id: r.Id, Model: r.Model, Name: r.Name, Configured: true}}, c.err
}
func (c *armTestClient) ForgetRobotArm(_ context.Context, r *pb.ForgetRobotArmRequest, _ ...grpc.CallOption) (*pb.ForgetRobotArmResponse, error) {
	c.forgotten = r.Id
	return &pb.ForgetRobotArmResponse{}, c.err
}

func TestUSBArmJSONListBindForgetAndOldAgent(t *testing.T) {
	client := &armTestClient{devices: []*pb.RobotArm{{Id: "target-only", DevicePath: "/target/dev/ttyACM8", Candidate: true, Connected: true, ConnectionStatus: "present", AccessStatus: "unknown"}}}
	var out bytes.Buffer
	if err := runArmPair(context.Background(), client, "", "", "", false, true, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || result.Devices[0]["device_path"] != "/target/dev/ttyACM8" || result.Devices[0]["access_status"] != "unknown" {
		t.Fatalf("target JSON: %s", out.String())
	}
	out.Reset()
	client.devices = nil
	if err := runArmPair(context.Background(), client, "", "", "", false, true, &out); err != nil {
		t.Fatal(err)
	}
	result.Devices = nil
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Devices == nil || len(result.Devices) != 0 {
		t.Fatalf("empty array contract: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runArmPair(context.Background(), client, "target-only", "", "Follower", true, true, &out); err != nil {
		t.Fatal(err)
	}
	var binding struct {
		Device map[string]any `json:"device"`
	}
	if err := json.Unmarshal(out.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if client.bound.Id != "target-only" || client.bound.Model != "so101" || !client.bound.AllowLocationBinding || client.bound.Name != "Follower" || binding.Device["verified"] != false || binding.Device["configured"] != true {
		t.Fatalf("bind: %+v %s", client.bound, out.String())
	}
	out.Reset()
	if err := runArmPair(context.Background(), client, "", "target-only", "", false, true, &out); err != nil || client.forgotten != "target-only" || !strings.Contains(out.String(), "forgotten") {
		t.Fatalf("forget: %s %v", out.String(), err)
	}
	client.err = status.Error(codes.Unimplemented, "unknown service")
	out.Reset()
	err := runArmPair(context.Background(), client, "", "", "", false, true, &out)
	if err == nil || !strings.Contains(err.Error(), "target Agent does not support") || out.Len() != 0 {
		t.Fatalf("old agent fallback: %s %v", out.String(), err)
	}
}
func TestUSBArmTabConfirmationRoutingAndOfflineForget(t *testing.T) {
	client := &armTestClient{devices: []*pb.RobotArm{{Id: "usb-1", DevicePath: "/target/tty", Connected: true, IdentityKind: "usb_location", ConnectionStatus: "present"}}}
	m := newDevicePairModel(&pairTestBluetooth{}, pairTestHandler(&pairTestClient{}), nil)
	m.arm = newArmPairModel(&armPairHandler{ctx: context.Background(), client: client})
	m.active = pairArmTab
	m, _ = pairUpdate(m, m.Init()())
	if client.scans != 1 || !strings.Contains(m.View(), "/target/tty") {
		t.Fatal("target arm inventory missing")
	}
	m, cmd := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !m.arm.confirming || client.bound != nil || !strings.Contains(m.View(), "replacement") {
		t.Fatal("weak identity wasn't explicitly acknowledged")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.done || m.arm.confirming {
		t.Fatal("escape should cancel USB confirmation")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, cmd = pairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil || !m.arm.busy {
		t.Fatal("confirmed binding not sent")
	}
	m, duplicate := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if duplicate != nil {
		t.Fatal("duplicate bind while busy")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, refresh := pairUpdate(m, cmd())
	if client.bound == nil || m.active != pairBluetoothTab || refresh == nil {
		t.Fatal("background result misrouted")
	}
	client.devices = []*pb.RobotArm{{Id: "usb-1", Name: "Offline arm", Model: "so101", Configured: true, ConnectionStatus: "disconnected"}}
	m, _ = pairUpdate(m, refresh())
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	// Returning starts a fresh scan; complete it before interacting.
	m, _ = pairUpdate(m, pairTabMsg{tab: pairArmTab, msg: armPairScanMsg{devices: client.devices}})
	m, cmd = pairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if cmd == nil {
		t.Fatal("offline binding cannot be forgotten")
	}
	m, _ = pairUpdate(m, cmd())
	if client.forgotten != "usb-1" {
		t.Fatal("wrong binding forgotten")
	}
}
func TestUSBArmScanFailureBlocksStaleBindingAndSanitizes(t *testing.T) {
	client := &armTestClient{err: status.Error(codes.Unimplemented, "unknown service")}
	m := newArmPairModel(&armPairHandler{ctx: context.Background(), client: client})
	bad := "USB\x1b[2J\r\u202e"
	m.devices = []*pb.RobotArm{{Id: bad, Name: bad, Product: bad, Note: bad, Connected: true, Configured: true}}
	m.refreshRows("")
	updated, _ := m.Update(m.Init()())
	m = updated.(armPairModel)
	if len(m.devices) != 1 || m.scanOK || !strings.Contains(m.View(), "target Agent does not support") {
		t.Fatal("scan failure lost saved display")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("stale bind after failed scan")
	}
	updated, forget := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = updated.(armPairModel)
	if forget == nil {
		t.Fatal("known saved binding cannot be forgotten after inventory failure")
	}
	updated, _ = m.Update(forget())
	m = updated.(armPairModel)
	if len(m.devices) != 1 || m.busy {
		t.Fatal("failed forget lost record or stayed busy")
	}
	view := m.View()
	if strings.ContainsAny(view, "\r\u202e") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("terminal control injection: %q", view)
	}
}
func TestUSBArmFlagsLeaveLegacyPairingUntouched(t *testing.T) {
	if err := validateArmPairFlags(false, true, "", "", "", false, true); err != nil {
		t.Fatalf("legacy --list --name rejected: %v", err)
	}
	for _, err := range []error{
		validateArmPairFlags(false, false, "id", "", "", false, false),
		validateArmPairFlags(true, false, "id", "id", "", false, false),
		validateArmPairFlags(true, true, "id", "", "", false, false),
		validateArmPairFlags(true, false, "", "", "", true, false),
		validateArmPairFlags(true, false, "", "", "", false, true),
	} {
		if err == nil {
			t.Fatal("incompatible flags accepted")
		}
	}
	cmd := newDevicePairCmd()
	if cmd.Flags().Lookup("list") == nil || cmd.Flags().Lookup("name") == nil || cmd.Flags().Lookup("sensors") == nil {
		t.Fatal("legacy flags removed")
	}
}
