package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// hostPortHolderFn names another running app that listens on the port, or "".
var hostPortHolderFn = func(ctx context.Context, conn *grpcclient.AgentConnection, self string, port int) string {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return ""
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF || err != nil {
			return ""
		}
		c := resp.GetContainer()
		if c == nil || c.GetAppName() == self || c.GetRunningState() != agentpb.AppRunningState_RUNNING {
			continue
		}
		if listens := devicePortListening(conn, c.GetAppName(), port); listens != nil {
			if ok, _ := listens(ctx); ok {
				return c.GetAppName()
			}
		}
	}
}

// hostPortAnswersFn reports whether the port answers on the device's own
// address, and whether this machine could check at all. A port inside another
// app's private network namespace does not answer there, and does not clash.
var hostPortAnswersFn = func(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, port int) (answers, checked bool) {
	host, _, ok := hostSideAppAddress(ctx, conn, appCfg)
	if !ok || host == "" {
		return false, false
	}
	c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, true
	}
	_ = c.Close()
	return true, true
}

// preflightHostPorts stops a deploy before the build when another app already
// serves this app's port on the device. It only refuses what it can confirm
// from here; the agent's own check at start decides the rest.
func preflightHostPorts(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) error {
	if appCfg == nil || conn == nil || conn.ContainerService == nil || !usesHostNetwork(appCfg.Entitlements) {
		return nil
	}
	// Flashing this app's sketch stops the app whose sketch is on the chip, so
	// that app's port is free by the time this one starts.
	replaced := ""
	if appCfg.MCU != nil {
		if v, err := agentVersionForRun(ctx, conn); err == nil {
			replaced = mcuSketchOwner(v)
		}
	}
	for _, e := range appCfg.Entitlements {
		if (e.Type != appconfig.EntitlementHTTP && e.Type != appconfig.EntitlementMCP) || e.Port <= 0 {
			continue
		}
		holder := hostPortHolderFn(ctx, conn, appCfg.AppID, e.Port)
		if holder == "" || holder == replaced {
			continue
		}
		answers, checked := hostPortAnswersFn(ctx, conn, appCfg, e.Port)
		switch {
		case answers:
			return withNextSteps(commandErrorf(errProjectTargetMismatch, "port %d on the device is already used by app %s", e.Port, holder),
				"Stop it: wendy device apps stop "+holder,
				"Or change this app's port in wendy.json.")
		case !checked:
			cliNotice("Notice: app %s also listens on port %d; if both use the device's network, this deploy will be refused.", holder, e.Port)
		}
	}
	return nil
}

func usesHostNetwork(ents []appconfig.Entitlement) bool {
	for _, e := range ents {
		if e.Type == appconfig.EntitlementNetwork && (e.Mode == "host" || e.Mode == "host-admin" || e.Mode == "") {
			return true
		}
	}
	return false
}

// startContainerError reports a failed start. The device's own refusal, such
// as a port another app holds, is shown as the device worded it.
func startContainerError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition {
		return commandErrorf(errProjectTargetMismatch, "the device refused to start the app: %s", st.Message())
	}
	return fmt.Errorf("starting container: %w", err)
}
