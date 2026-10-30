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

// runRefusals are the failures the CLI reports before it sends anything to
// the device, by the phrase that begins each message, with what the tool says
// instead of its generic failure text: unlike other failures, there is no
// device state to inspect afterwards.
var runRefusals = []struct{ phrase, message, nextStep string }{
	{
		// checkLocalBuildSpace, checkDeviceDiskSpace and deviceImageSpaceError
		// in package commands.
		phrase:   "not enough free disk space",
		message:  "Nothing was deployed: there is too little free disk space for this run, on this computer or on the device. The output names the disk and ways to free space.",
		nextStep: "Show the user the ways to free disk space in the output and ask before deleting anything, then run again.",
	},
	{
		// checkAgentEntitlements in package commands.
		phrase:   "the device's agent does not support",
		message:  "Nothing was deployed: the device's agent does not support an entitlement in wendy.json and would start the app without that access. The output names the entitlement.",
		nextStep: "Ask the user whether to update the device (wendy device update) or remove the entitlement from wendy.json, then run again.",
	},
}

// runRefusal returns what the tool reports when a failed `wendy run` child
// stopped at one of runRefusals, and ok=false for any other failure.
func runRefusal(output string) (message, nextStep string, ok bool) {
	if len(output) > runDiagnosticWindow {
		output = output[len(output)-runDiagnosticWindow:]
	}
	for _, r := range runRefusals {
		if strings.Contains(output, r.phrase) {
			return r.message, r.nextStep, true
		}
	}
	return "", "", false
}

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
