package containerd

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// hostPortBusy reports whether something already listens on the TCP port in
// the agent's (the host's) network namespace.
var hostPortBusy = func(port int) bool {
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// declaredHostPorts returns the TCP ports a host-network app says it serves.
func declaredHostPorts(ents []appconfig.Entitlement) []int {
	if !entitlementsUseHostNetwork(ents) {
		return nil
	}
	var ports []int
	for _, e := range ents {
		if (e.Type == appconfig.EntitlementHTTP || e.Type == appconfig.EntitlementMCP) && e.Port > 0 {
			ports = append(ports, e.Port)
		}
	}
	return ports
}

// checkHostPorts refuses an app whose declared port another app or process
// already holds: it would start, fail to bind and crash-loop. The app's own
// listeners don't count, so a redeploy over a running version passes.
func (c *Client) checkHostPorts(ctx context.Context, appID string, ents []appconfig.Entitlement) error {
	for _, port := range declaredHostPorts(ents) {
		if !hostPortBusy(port) {
			continue
		}
		holder, err := c.hostPortHolder(ctx, port)
		if err != nil || holder == appID {
			continue
		}
		who := "another process on the device"
		if holder != "" {
			who = "app " + holder
		}
		return status.Error(codes.FailedPrecondition,
			fmt.Sprintf("port %d is already used by %s; stop it or change this app's port in wendy.json", port, who))
	}
	return nil
}

// hostPortHolder names the app whose processes listen on the port, or "".
func (c *Client) hostPortHolder(ctx context.Context, port int) (string, error) {
	ctrs, err := c.client.Containers(c.withNamespace(ctx), fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(c.withNamespace(ctx))
		if err != nil {
			continue
		}
		appID := labels[labelKeyAppID]
		if appID == "" || seen[appID] {
			continue
		}
		seen[appID] = true
		entries, err := c.GetListeningPorts(ctx, appID)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.GetProtocol(), "tcp") && int(e.GetPort()) == port {
				return appID, nil
			}
		}
	}
	return "", nil
}
