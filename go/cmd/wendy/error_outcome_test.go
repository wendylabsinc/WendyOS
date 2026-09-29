package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// outcomeCases lists every class errorClass and commands.ErrorClass can
// return, plus the pendingClasses. TestErrorOutcomesMatchTheClassifiers
// fails until a new class has a row here.
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

	{"internal_error", 70, false},

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

// pendingClasses are returned by classifiers that have not landed yet:
// "app_crashed" and "terminated" arrive with the wendy run outcome work.
// Remove an entry once its classifier returns it.
var pendingClasses = map[string]bool{"app_crashed": true, "terminated": true}

// classifierClasses returns every class string the classifiers can return,
// read from the string literals in their return statements: errorClass in
// main.go and commands.ErrorClass.
func classifierClasses(t *testing.T) map[string]bool {
	t.Helper()
	classes := map[string]bool{}
	for _, src := range []struct{ file, fn string }{
		{"main.go", "errorClass"},
		{"../../internal/cli/commands/errorclass.go", "ExecutionErrorClass"},
		{"../../internal/cli/commands/error_class.go", "ErrorClass"},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), src.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != src.fn {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) != 1 {
					return true
				}
				if lit, ok := ret.Results[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					class, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					classes[class] = true
				}
				return true
			})
		}
		if !found {
			t.Fatalf("%s: no func %s; update classifierClasses", src.file, src.fn)
		}
	}
	if len(classes) < 20 {
		t.Fatalf("found only %d classes; the classifiers changed shape, update classifierClasses", len(classes))
	}
	return classes
}

// Every class a classifier returns must have a deliberate row in
// outcomeCases, and the table must not name a class no classifier returns
// (a misspelt key would silently leave its class at exit 1).
func TestErrorOutcomesMatchTheClassifiers(t *testing.T) {
	known := classifierClasses(t)
	rows := make(map[string]bool, len(outcomeCases))
	for _, tc := range outcomeCases {
		rows[tc.class] = true
		if !known[tc.class] && !pendingClasses[tc.class] {
			t.Errorf("outcomeCases has %q, which no classifier returns", tc.class)
		}
	}
	for class := range known {
		if !rows[class] {
			t.Errorf("classifiers return %q, which has no outcomeCases row; decide its exit status", class)
		}
	}
	for class := range errorOutcomes {
		if !known[class] && !pendingClasses[class] {
			t.Errorf("errorOutcomes has %q, which no classifier returns; typo?", class)
		}
	}
	for class := range pendingClasses {
		if known[class] {
			t.Errorf("%q is returned by a classifier now; remove it from pendingClasses", class)
		}
	}
}
