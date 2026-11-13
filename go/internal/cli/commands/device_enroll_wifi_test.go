package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type enrollmentWifiClient struct {
	agentpb.WendyAgentServiceClient
	response *agentpb.GetWiFiStatusResponse
	err      error
}

func (c *enrollmentWifiClient) GetWiFiStatus(context.Context, *agentpb.GetWiFiStatusRequest, ...grpc.CallOption) (*agentpb.GetWiFiStatusResponse, error) {
	return c.response, c.err
}

func TestEnrollmentWifi(t *testing.T) {
	noAdapter := "no WiFi device found"
	for _, tc := range []struct {
		name        string
		response    *agentpb.GetWiFiStatusResponse
		statusErr   error
		confirmErr  error
		pickErr     error
		passwordErr error
		decline     bool
		wantPrompt  bool
		wantCancel  bool
	}{
		{name: "no adapter", response: &agentpb.GetWiFiStatusResponse{ErrorMessage: &noAdapter}},
		{name: "unsupported", statusErr: errors.New("unsupported")},
		{name: "connected", response: &agentpb.GetWiFiStatusResponse{Connected: true}},
		{name: "decline", response: &agentpb.GetWiFiStatusResponse{}, decline: true, wantPrompt: true},
		{name: "cancel confirmation", response: &agentpb.GetWiFiStatusResponse{}, confirmErr: tui.ErrCancelled, wantPrompt: true, wantCancel: true},
		{name: "cancel picker", response: &agentpb.GetWiFiStatusResponse{}, pickErr: ErrUserCancelled, wantPrompt: true, wantCancel: true},
		{name: "cancel password", response: &agentpb.GetWiFiStatusResponse{}, passwordErr: tui.ErrCancelled, wantPrompt: true, wantCancel: true},
		{name: "scan failure", response: &agentpb.GetWiFiStatusResponse{}, pickErr: errors.New("scan failed"), wantPrompt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interactive, confirm, pick, password := isInteractiveTerminalFn, confirmEnrollmentWifiFn, pickEnrollmentWifiFn, promptWifiPassword
			t.Cleanup(func() {
				isInteractiveTerminalFn, confirmEnrollmentWifiFn, pickEnrollmentWifiFn, promptWifiPassword = interactive, confirm, pick, password
			})
			isInteractiveTerminalFn = func() bool { return true }
			prompted := false
			confirmEnrollmentWifiFn = func() (bool, error) { prompted = true; return !tc.decline, tc.confirmErr }
			pickEnrollmentWifiFn = func(context.Context, *SelectedDevice) (string, error) { return "test", tc.pickErr }
			promptWifiPassword = func(string) (string, error) { return "", tc.passwordErr }
			conn := &grpcclient.AgentConnection{AgentService: &enrollmentWifiClient{response: tc.response, err: tc.statusErr}}
			err := promptWifiIfNeeded(context.Background(), conn)
			if errors.Is(err, ErrUserCancelled) != tc.wantCancel {
				t.Fatalf("error = %v, want cancellation %v", err, tc.wantCancel)
			}
			if err != nil && !tc.wantCancel {
				t.Fatal(err)
			}
			if prompted != tc.wantPrompt {
				t.Fatalf("prompted = %v, want %v", prompted, tc.wantPrompt)
			}
		})
	}
}
