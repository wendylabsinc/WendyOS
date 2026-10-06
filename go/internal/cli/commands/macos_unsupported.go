package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const macOSBetaUnsupportedSuffix = "is not available in the current Wendy Agent for macOS beta."

// diagnosticAgentVersion reuses the version observed during connection when
// possible. A failed probe returns nil for the caller to handle conservatively.
func diagnosticAgentVersion(ctx context.Context, conn *grpcclient.AgentConnection) *agentpb.GetAgentVersionResponse {
	if conn == nil {
		return nil
	}
	if version, ok := conn.CachedAgentVersion(); ok {
		return version
	}
	if conn.AgentService == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	version, err := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil
	}
	return version
}

// The native-process feature identifies the Swift macOS agent. A Go agent can
// also run on Darwin, but it registers a different set of services.
func isSwiftMacAgent(version *agentpb.GetAgentVersionResponse) bool {
	return strings.EqualFold(version.GetOs(), "darwin") && agentVersionHasFeature(version, "native-process")
}

func diagnosticContextError(ctx context.Context) error {
	if ctx.Err() == context.Canceled {
		return ErrUserCancelled
	}
	return ctx.Err()
}

func macOSBetaUnsupportedFeatureError(ctx context.Context, agent agentpb.WendyAgentServiceClient, err error, feature string) error {
	if agent == nil || status.Code(err) != codes.Unimplemented {
		return nil
	}

	resp, versionErr := agent.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if versionErr != nil || !strings.EqualFold(resp.GetOs(), "darwin") {
		return nil
	}

	return fmt.Errorf("%s %s", feature, macOSBetaUnsupportedSuffix)
}
