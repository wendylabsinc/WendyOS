//go:build !linux

package hoststats

import (
	"context"
	"fmt"
)

func EnableAgentNetworkAccounting(context.Context) error {
	return fmt.Errorf("agent IP accounting requires Linux")
}
func ReadNetwork(context.Context) NetworkSample {
	return NetworkSample{InterfacesError: "Device network counters require Linux.", AgentError: "Agent service IP counters require Linux."}
}
