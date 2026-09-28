package main

// errorOutcome is what a failure class tells the caller: the process exit
// status, and whether running the identical command again can succeed.
type errorOutcome struct {
	exit      int
	retryable bool
}

// Exit statuses. They are a published contract (see "Exit status" in
// docs/clients/wendy-cli/global-flags.md): add new ones, never renumber.
const (
	exitFailure     = 1   // unclassified, or a class with no status of its own
	exitUsage       = 2   // the command line itself is wrong; fix it, do not retry
	exitAuth        = 3   // credentials are missing, expired, or ambiguous
	exitTarget      = 4   // no device, several candidates, or the wrong kind of device
	exitUnreachable = 5   // the device could not be reached
	exitBuild       = 6   // building the app failed, or a build tool is missing
	exitAppFailed   = 7   // the app was deployed but did not start or stay up
	exitNotReady    = 8   // the app started but never became ready
	exitNeedsHuman  = 10  // a trust decision only a person can make
	exitTerminated  = 143 // stopped by SIGTERM (128 + 15)
)

// errorOutcomes maps an errorClass value to its outcome. It is keyed by the
// class string rather than by sentinel error, so a class defined elsewhere
// ("app_crashed" and "terminated" arrive with the wendy run outcome work)
// takes effect here as soon as commands.ErrorClass starts returning it.
// A class missing from the table exits exitFailure and is not retryable.
var errorOutcomes = map[string]errorOutcome{
	"cli_usage": {exit: exitUsage},

	"auth_required":           {exit: exitAuth},
	"auth_session_ambiguous":  {exit: exitAuth},
	"auth_certificate_failed": {exit: exitAuth},
	"device_auth_required":    {exit: exitAuth},
	"registry_auth":           {exit: exitAuth},
	"grpc_unauthenticated":    {exit: exitAuth},

	"no_device":               {exit: exitTarget},
	"device_ambiguous":        {exit: exitTarget},
	"project_target_mismatch": {exit: exitTarget},

	"device_unreachable":    {exit: exitUnreachable, retryable: true},
	"device_offline":        {exit: exitUnreachable, retryable: true},
	"device_tls_rejected":   {exit: exitUnreachable},
	"simulator_unavailable": {exit: exitUnreachable},

	"build_failed":        {exit: exitBuild},
	"builder_unavailable": {exit: exitBuild},
	"tool_not_found":      {exit: exitBuild},

	"container_start_failed": {exit: exitAppFailed},
	"app_crashed":            {exit: exitAppFailed},

	"readiness_timeout": {exit: exitNotReady, retryable: true},

	"device_identity_mismatch": {exit: exitNeedsHuman},
	"device_org_mismatch":      {exit: exitNeedsHuman},

	"terminated": {exit: exitTerminated},

	// Transient failures without a status of their own.
	"transfer_failed":      {exit: exitFailure, retryable: true},
	"registry_unavailable": {exit: exitFailure, retryable: true},
	"grpc_deadline":        {exit: exitFailure, retryable: true},
	"network_timeout":      {exit: exitFailure, retryable: true},
}

// outcomeForClass returns the outcome for an errorClass value.
func outcomeForClass(class string) errorOutcome {
	if outcome, ok := errorOutcomes[class]; ok {
		return outcome
	}
	return errorOutcome{exit: exitFailure}
}
