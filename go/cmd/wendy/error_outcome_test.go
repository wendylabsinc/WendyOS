package main

import "testing"

// outcomeCases lists every class errorClass and commands.ErrorClass can
// return, plus "app_crashed" and "terminated", which the wendy run outcome
// work adds to commands.ErrorClass. A new class belongs here too.
var outcomeCases = []struct {
	class     string
	exit      int
	retryable bool
}{
	{"cli_usage", 2, false},

	{"auth_required", 3, false},
	{"auth_session_ambiguous", 3, false},
	{"auth_certificate_failed", 3, false},
	{"device_auth_required", 3, false},
	{"registry_auth", 3, false},
	{"grpc_unauthenticated", 3, false},

	{"no_device", 4, false},
	{"device_ambiguous", 4, false},
	{"project_target_mismatch", 4, false},

	{"device_unreachable", 5, true},
	{"device_offline", 5, true},
	{"device_not_resolved", 5, false},
	{"device_tls_rejected", 5, false},
	{"simulator_unavailable", 5, false},

	{"build_failed", 6, false},
	{"builder_unavailable", 6, false},
	{"tool_not_found", 6, false},

	{"container_start_failed", 7, false},
	{"app_crashed", 7, false},

	{"readiness_timeout", 8, true},

	{"device_identity_mismatch", 10, false},
	{"device_org_mismatch", 10, false},

	{"terminated", 143, false},

	{"transfer_failed", 1, true},
	{"registry_unavailable", 1, true},
	// A timed-out call or connection can succeed when run again.
	{"grpc_deadline", 1, true},
	{"network_timeout", 1, true},

	// Everything else is a plain, non-retryable failure.
	{"other", 1, false},
	{"user_cancelled", 1, false},
	{"config_invalid", 1, false},
	{"context_canceled", 1, false},
	{"context_deadline", 1, false},
	{"grpc_unavailable", 1, false},
	{"grpc_unimplemented", 1, false},
	{"grpc_invalid_argument", 1, false},
	{"grpc_not_found", 1, false},
	{"grpc_already_exists", 1, false},
	{"grpc_permission_denied", 1, false},
	{"grpc_resource_exhausted", 1, false},
	{"grpc_failed_precondition", 1, false},
	{"grpc_aborted", 1, false},
	{"grpc_out_of_range", 1, false},
	{"grpc_internal", 1, false},
	{"grpc_data_loss", 1, false},
	{"grpc_unknown", 1, false},
	{"grpc_other", 1, false},
	{"network_dns", 1, false},
	{"network_refused", 1, false},
	{"network_unreachable", 1, false},
	{"connection_closed", 1, false},
	{"permission_denied", 1, false},
	{"file_not_found", 1, false},
	{"disk_full", 1, false},
	{"unexpected_eof", 1, false},
	{"process_failed", 1, false},
	{"", 1, false},
}

func TestOutcomeForClass(t *testing.T) {
	for _, tc := range outcomeCases {
		got := outcomeForClass(tc.class)
		if got.exit != tc.exit || got.retryable != tc.retryable {
			t.Errorf("outcomeForClass(%q) = {exit %d, retryable %v}, want {exit %d, retryable %v}",
				tc.class, got.exit, got.retryable, tc.exit, tc.retryable)
		}
	}
}

// A misspelt key in errorOutcomes would silently leave its class at exit 1.
func TestErrorOutcomesOnlyNameKnownClasses(t *testing.T) {
	known := make(map[string]bool, len(outcomeCases))
	for _, tc := range outcomeCases {
		known[tc.class] = true
	}
	for class := range errorOutcomes {
		if !known[class] {
			t.Errorf("errorOutcomes has %q, which no classifier returns; typo, or add it to outcomeCases", class)
		}
	}
}
