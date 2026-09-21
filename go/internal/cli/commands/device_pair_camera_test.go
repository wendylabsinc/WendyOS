package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cameraPairTestClient struct {
	devices    []*agentpb.VideoDevice
	scans      int
	saved      *agentpb.SetCameraCredentialsRequest
	forgot     uint32
	err        error
	refreshErr error
	lists      int
}

func (c *cameraPairTestClient) RefreshCameras(context.Context, *agentpb.RefreshCamerasRequest, ...grpc.CallOption) (*agentpb.RefreshCamerasResponse, error) {
	c.scans++
	if c.refreshErr != nil {
		return nil, c.refreshErr
	}
	return &agentpb.RefreshCamerasResponse{Devices: c.devices}, c.err
}

func (c *cameraPairTestClient) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error) {
	c.lists++
	return &agentpb.ListVideoDevicesResponse{Devices: c.devices}, c.err
}

func TestCameraPairDiscoveryFallsBackForOlderAgents(t *testing.T) {
	c := &cameraPairTestClient{refreshErr: status.Error(codes.Unimplemented, "older agent"), devices: []*agentpb.VideoDevice{{Id: 42}}}
	h := &cameraPairHandler{ctx: context.Background(), client: c}
	msg := h.scan()().(cameraPairScanMsg)
	if msg.err != nil || len(msg.devices) != 1 || c.scans != 1 || c.lists != 1 {
		t.Fatalf("discovery fallback failed: %+v", msg)
	}
}
func (c *cameraPairTestClient) SetCameraCredentials(_ context.Context, req *agentpb.SetCameraCredentialsRequest, _ ...grpc.CallOption) (*agentpb.SetCameraCredentialsResponse, error) {
	c.saved = req
	return &agentpb.SetCameraCredentialsResponse{}, c.err
}
func (c *cameraPairTestClient) ForgetCamera(_ context.Context, req *agentpb.ForgetCameraRequest, _ ...grpc.CallOption) (*agentpb.ForgetCameraResponse, error) {
	c.forgot = req.DeviceId
	return &agentpb.ForgetCameraResponse{}, c.err
}

func cameraPairUpdate(m cameraPairModel, msg tea.Msg) (cameraPairModel, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(cameraPairModel), cmd
}

func TestCameraPairDiscoveryFiltersLocalAndRetainsOfflineCameras(t *testing.T) {
	c := &cameraPairTestClient{devices: []*agentpb.VideoDevice{
		{Id: 1, Name: "USB camera"},
		{Id: 2, Name: "IP camera", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, Online: true},
		{Id: 3, Name: "Saved camera", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, HasCredentials: true},
	}}
	m := newCameraPairModel(&cameraPairHandler{ctx: context.Background(), client: c})
	m, _ = cameraPairUpdate(m, m.Init()())
	if len(m.devices) != 2 || m.devices[0].Id != 3 || strings.Contains(m.View(), "USB camera") || !strings.Contains(m.View(), "offline") {
		t.Fatalf("camera discovery did not filter and retain devices correctly: %s", m.View())
	}
	c.err = errors.New("device disconnected")
	m, _ = cameraPairUpdate(m, m.Init()())
	if len(m.devices) != 2 || !strings.Contains(m.View(), "device disconnected") {
		t.Fatal("failed discovery discarded known cameras or hid its error")
	}
	c.err = nil
	m, forget := cameraPairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m, _ = cameraPairUpdate(m, forget())
	if c.forgot != 3 || len(m.devices) != 1 || m.devices[0].Id != 2 {
		t.Fatal("offline camera could not be forgotten")
	}
}

func TestDevicePairCameraTabCredentialFormAndBackgroundResults(t *testing.T) {
	c := &cameraPairTestClient{devices: []*agentpb.VideoDevice{
		{Id: 42, Name: "Door camera", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, Online: true},
	}}
	m := newDevicePairModel(&pairTestBluetooth{}, pairTestHandler(&pairTestClient{}), &cameraPairHandler{ctx: context.Background(), client: c})
	// Shift+Tab wraps from Bluetooth to the last tab.
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.active != pairCameraTab || scan == nil || c.scans != 0 {
		t.Fatal("camera tab did not start lazy discovery")
	}
	m, _ = pairUpdate(m, scan())
	if !strings.Contains(m.View(), "Door camera") || c.scans != 1 {
		t.Fatal("camera discovery did not render")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.active != pairCameraTab || !m.camera.passwordFocused {
		t.Fatal("Tab switched tabs instead of credential fields")
	}
	secret := "qfr-secret"
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(secret)})
	if strings.Contains(m.View(), secret) || m.done {
		t.Fatal("password was shown or treated as a keyboard shortcut")
	}
	m, save := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if save == nil || m.camera.password.Value() != "" || !m.camera.busy {
		t.Fatal("submitting credentials did not clear the password or start saving")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.active != pairBluetoothTab {
		t.Fatal("Tab did not wrap from camera to Bluetooth")
	}
	m, _ = pairUpdate(m, save())
	if c.saved == nil || c.saved.DeviceId != 42 || c.saved.Username != "admin" || c.saved.Password != secret || !m.camera.devices[0].HasCredentials || m.active != pairBluetoothTab {
		t.Fatal("camera save result was not routed to its background tab")
	}
	// Revisit, edit and cancel without exiting the surrounding picker.
	m, rescan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	m, _ = pairUpdate(m, rescan())
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, quit := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	if quit != nil || m.done || m.camera.editing || m.active != pairCameraTab {
		t.Fatal("Escape did not return from credentials to the camera list")
	}
}

func TestCameraPairOperationFailurePreservesState(t *testing.T) {
	m := newCameraPairModel(nil)
	m, _ = cameraPairUpdate(m, cameraPairScanMsg{devices: []*agentpb.VideoDevice{{Id: 42, Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP}}})
	m.busy = true
	m, _ = cameraPairUpdate(m, cameraPairOpMsg{id: 42, err: errors.New("save failed")})
	if m.busy || m.devices[0].HasCredentials || !strings.Contains(m.message, "save failed") {
		t.Fatal("failed save changed pairing state")
	}
	m, _ = cameraPairUpdate(m, cameraPairOpMsg{id: 42, forgot: true, err: errors.New("forget failed")})
	if len(m.devices) != 1 || !strings.Contains(m.message, "forget failed") {
		t.Fatal("failed forget removed a camera")
	}
}

func TestCameraPairFailureDoesNotExposeRemoteCredentialDetails(t *testing.T) {
	for _, detail := range []string{"could not store secret-value", "secret%2Dvalue", "c2VjcmV0LXZhbHVl", "partial secret"} {
		c := &cameraPairTestClient{err: status.Error(codes.Internal, detail)}
		h := &cameraPairHandler{ctx: context.Background(), client: c}
		msg := h.pair(42, "admin", "secret-value")().(cameraPairOpMsg)
		if msg.err == nil || msg.err.Error() != "saving camera login failed" {
			t.Fatalf("camera error exposed remote details: %v", msg.err)
		}
	}
}
