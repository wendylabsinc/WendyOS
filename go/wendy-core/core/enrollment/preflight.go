package enrollment

import (
	"context"
	"fmt"

	agentpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/agentpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// CheckAgentEnrollment checks readiness without generating keys, reserving a
// Cloud asset or presenting EAB material. StartACMEProvisioning still enforces
// provisioning state and validates enrollment material after this read.
func CheckAgentEnrollment(ctx context.Context, transport platform.Transport) error {
	resp := new(agentpbv2.IsProvisionedResponse)
	err := unary(ctx, transport, "/wendy.agent.services.v2.WendyProvisioningService/IsProvisioned", &agentpbv2.IsProvisionedRequest{}, resp)
	if rpcCode(err) == platform.RPCUnimplemented {
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent before enrolling")
	}
	if err != nil {
		return fmt.Errorf("checking device enrollment: %w", err)
	}
	if resp.GetProvisioned() != nil {
		return fmt.Errorf("device is already enrolled; no new enrollment was started")
	}
	state := resp.GetNotProvisioned()
	if state == nil {
		return fmt.Errorf("agent returned an unknown enrollment state")
	}
	if state.AcmeEnrollmentSupported == nil {
		return fmt.Errorf("this agent does not advertise direct PKI enrollment readiness; update the agent before enrolling (no Cloud reservation made)")
	}
	if !state.GetAcmeEnrollmentSupported() {
		// Agent-supplied text: %q keeps it to one escaped line in the terminal.
		if reason := state.GetAcmeEnrollmentUnavailableReason(); reason != "" {
			return fmt.Errorf("this device cannot enroll directly with PKI: %q (no Cloud reservation made)", reason)
		}
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent before enrolling (no Cloud reservation made)")
	}
	return nil
}
