package bluetooth

import (
	"context"
	"sync"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const SetupServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b632"
const SetupPSM = 129

type setupProvisioner interface {
	IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error)
	StartACMEProvisioning(context.Context, *agentpbv2.StartACMEProvisioningRequest) (*agentpbv2.StartACMEProvisioningResponse, error)
}

// SetupDispatcher exposes only first-enrollment operations. Never use the
// management Dispatcher directly on an unauthenticated connection.
type SetupDispatcher struct {
	mu           sync.Mutex
	provisioning setupProvisioner
	wifi         *Dispatcher
}

func NewSetupDispatcher(p setupProvisioner, n networkOps) *SetupDispatcher {
	return &SetupDispatcher{provisioning: p, wifi: NewDispatcher(n, nil, nil, nil)}
}

func (d *SetupDispatcher) Available(ctx context.Context) bool {
	status, err := d.provisioning.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	return err == nil && status.GetNotProvisioned() != nil && status.GetNotProvisioned().GetAcmeEnrollmentSupported()
}

func (d *SetupDispatcher) Dispatch(ctx context.Context, cmd *agentpb.BluetoothCommand) *agentpb.BluetoothResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, err := d.provisioning.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil {
		return errResp("Setup status unavailable")
	}
	if cmd.GetSetupStatus() != nil {
		status := &agentpb.SetupStatusResponse{Enrolled: state.GetProvisioned() != nil, AcmeSupported: state.GetNotProvisioned().GetAcmeEnrollmentSupported()}
		if p := state.GetProvisioned(); p != nil {
			status.PrincipalUri = p.GetPrincipalUri()
		}
		return &agentpb.BluetoothResponse{Response: &agentpb.BluetoothResponse_SetupStatus{SetupStatus: status}}
	}
	if state.GetNotProvisioned() == nil || !state.GetNotProvisioned().GetAcmeEnrollmentSupported() {
		return errResp("Device is not available for setup")
	}
	switch c := cmd.Command.(type) {
	case *agentpb.BluetoothCommand_WifiList, *agentpb.BluetoothCommand_WifiStatus, *agentpb.BluetoothCommand_WifiConnect:
		return d.wifi.Dispatch(ctx, cmd)
	case *agentpb.BluetoothCommand_SetupEnroll:
		r := c.SetupEnroll
		enrolled, err := d.provisioning.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
			CloudHost: r.GetCloudHost(), DirectoryUrl: r.GetDirectoryUrl(), DeviceId: r.GetDeviceId(), EabKeyId: r.GetEabKeyId(), EabHmacKey: r.GetEabHmacKey(),
		})
		// Errors from PKI or network libraries may contain credential material.
		if err != nil {
			return errResp("Enrollment could not be completed. Check the device's internet connection and retry.")
		}
		return &agentpb.BluetoothResponse{Response: &agentpb.BluetoothResponse_SetupEnroll{SetupEnroll: &agentpb.SetupEnrollResponse{PrincipalUri: enrolled.GetPrincipalUri()}}}
	default:
		return errResp("Command unavailable during setup")
	}
}
