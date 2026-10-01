package mcp

import (
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// runTargetErrResult maps a runTarget failure to an error code. With no
// connection and no explicit selector there is nothing to deploy to: report
// NOT_CONNECTED, the code every device tool uses, rather than blaming the
// arguments. Everything else is a genuine argument problem.
func (s *mcpServer) runTargetErrResult(req mcpgo.CallToolRequest, err error) *mcpgo.CallToolResult {
	conn, _, _ := s.connectionSnapshot()
	if conn == nil && !runHasExplicitTarget(req) {
		return errResult(errCodeNotConnected, "no device connected and no device given: call device_list, then device_connect with a returned device selector, or pass it as device (host:port, vm:NAME or a cloud:// selector)")
	}
	return errResult(errCodeInvalidArgument, err.Error())
}

func runHasExplicitTarget(req mcpgo.CallToolRequest) bool {
	for _, key := range []string{"device", "device_name", "cloud_grpc", "broker_url"} {
		if stringParam(req, key) != "" {
			return true
		}
	}
	return false
}

// runDiagnosticWindow bounds how much of the output tail is searched. The CLI
// prints its final error last, so only the end can hold it; searching the
// whole tail would let a build log that merely mentions a phrase decide the code.
const runDiagnosticWindow = 1024

// runFailureCode classifies a failed `wendy run` child from its final
// diagnostic. The CLI reports errors as text on stderr, so match the stable
// messages it prints for credential problems; anything else stays INTERNAL.
func runFailureCode(output string) errorCode {
	if len(output) > runDiagnosticWindow {
		output = output[len(output)-runDiagnosticWindow:]
	}
	switch {
	case strings.Contains(output, config.ErrMultipleSessions.Error()):
		return errCodeMultipleSessions
	case strings.Contains(output, "wendy auth login"), strings.Contains(output, "no login for cloud"):
		// "not logged in; run 'wendy auth login' ...", the provisioned agent's
		// "Unauthorized. Run 'wendy auth login' ...", and a cloud selector whose
		// org/tenant has no stored login.
		return errCodeAuthRequired
	default:
		return errCodeInternal
	}
}
