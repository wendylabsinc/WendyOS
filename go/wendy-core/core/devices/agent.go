package devices

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/devices/internal/agentpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

// AgentVersion is the Agent's existing version/capability response. The wire
// descriptor is private so hosts can link their own generated Agent messages.
type AgentVersion = agentpb.GetAgentVersionResponse

// GetAgentVersion probes a device over its host-owned transport. A successful
// RPC proves liveness after the host verifier accepts the peer, including on
// lazy connections. Authentication and remote status errors retain their cause.
func GetAgentVersion(ctx context.Context, transport platform.Transport) (*AgentVersion, error) {
	request, err := proto.Marshal(&agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil, err
	}
	response, err := transport.Unary(ctx, platform.RPCRequest{
		Method:  "/wendy.agent.services.v1.WendyAgentService/GetAgentVersion",
		Message: request,
	})
	if err != nil {
		return nil, err
	}
	version := new(AgentVersion)
	if err := proto.Unmarshal(response.Message, version); err != nil {
		return nil, fmt.Errorf("decoding Agent version: %w", err)
	}
	return version, nil
}
