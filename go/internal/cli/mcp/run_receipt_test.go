package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRunVerifiedReceiptAndServiceScope(t *testing.T) {
	s := New(&config.Config{}, nil)
	var argv []string
	s.runCommandFn = func(_ context.Context, args []string, _ commandTarget, _ int) (string, bool, error) {
		argv = args
		return encodeRunReceipt([]byte(`{"status":"ready","app":"robot_api","device":"127.0.0.1","readiness":"passed","url":"http://127.0.0.1:8080"}`), strings.Repeat("build log\n", 2000)), true, nil
	}
	result, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{
		"project_path": runProject(t), "device": "vm:robot", "service": "api", "wait_ready": true, "readiness_timeout_seconds": 20,
	}))
	if err != nil || result.IsError {
		t.Fatalf("run: %v %v", result, err)
	}
	for _, flag := range []string{"--service", "api", "--json", "--detach", "--wait-ready", "--readiness-timeout", "20s"} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("missing %s: %v", flag, argv)
		}
	}
	out := structuredMap(t, result)
	if out["status"] != "ready" || out["readiness"] != "passed" || out["app"] != "robot_api" || out["requested_service"] != "api" || out["output"] != nil {
		t.Fatalf("receipt: %+v", out)
	}
	encoded, _ := json.Marshal(out)
	if len(encoded) > 1000 {
		t.Fatalf("successful receipt unexpectedly large: %d bytes", len(encoded))
	}
	if out["target"].(commandTarget).Device != "vm:robot" {
		t.Fatal("CLI display address replaced selected target")
	}
}

func TestRunReadinessDefault(t *testing.T) {
	for _, tc := range []struct {
		name          string
		extra         map[string]any
		wait, invalid bool
	}{
		{"omitted", nil, true, false},
		{"deadline without flag", map[string]any{"readiness_timeout_seconds": 20}, true, false},
		{"explicit opt out", map[string]any{"wait_ready": false}, false, false},
		{"create only", map[string]any{"start": false}, false, false},
		{"legacy create only", map[string]any{"deploy": true}, false, false},
		{"docker requires opt out", map[string]any{"device": "docker"}, false, true},
		{"podman requires opt out", map[string]any{"device": "podman"}, false, true},
		{"docker opt out", map[string]any{"device": "docker", "wait_ready": false}, false, false},
		{"podman opt out", map[string]any{"device": "podman", "wait_ready": false}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&config.Config{}, nil)
			called := false
			s.runCommandFn = func(_ context.Context, argv []string, _ commandTarget, _ int) (string, bool, error) {
				called = true
				if tc.invalid || slices.Contains(argv, "--wait-ready") != tc.wait {
					t.Fatalf("unexpected deployment flags: %v", argv)
				}
				return encodeRunReceipt([]byte(`{"status":"ready","app":"robot","readiness":"passed"}`), ""), false, nil
			}
			args := map[string]any{"project_path": runProject(t), "device": "vm:test"}
			for key, value := range tc.extra {
				args[key] = value
			}
			result, err := s.handleRun(context.Background(), callToolReq("run", args))
			if err != nil || result.IsError != tc.invalid || called == tc.invalid {
				t.Fatalf("run: %v %v (called=%v)", result, err, called)
			}
			if !tc.invalid && (structuredMap(t, result)["readiness"] == "passed") != tc.wait {
				t.Fatalf("incorrect readiness: %v", result)
			}
		})
	}
}

func TestRunReadinessValidationBeforeMutation(t *testing.T) {
	for _, extra := range []map[string]any{
		{"wait_ready": "true"}, {"include_output": 1}, {"service": "api,other"}, {"service": 1},
		{"wait_ready": true, "start": false}, {"wait_ready": false, "readiness_timeout_seconds": 10},
		{"wait_ready": true, "readiness_timeout_seconds": 301}, {"wait_ready": true, "device": "docker"},
	} {
		s := New(&config.Config{}, nil)
		s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
			t.Fatal("invalid request started a deployment")
			return "", false, nil
		}
		args := map[string]any{"project_path": runProject(t), "device": "vm:test"}
		for k, v := range extra {
			args[k] = v
		}
		r, err := s.handleRun(context.Background(), callToolReq("run", args))
		if err != nil || !r.IsError {
			t.Fatalf("accepted invalid request %v: %v %v", extra, r, err)
		}
	}
}

func TestRunMissingAndFailedReadinessCannotPass(t *testing.T) {
	for _, tc := range []struct {
		stdout string
		err    error
		status string
	}{
		{"", nil, "failed"},
		{`{"status":"started","app":"robot","readiness":"not_checked"}`, nil, "failed"},
		{`{"status":"crashed","app":"robot","readiness":"failed","exit_code":7,"message":"missing configuration"}`, errors.New("exit status 1"), "crashed"},
		{`{"status":"not_ready","app":"robot","readiness":"failed"}`, nil, "not_ready"},
	} {
		s := New(&config.Config{}, nil)
		s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
			return encodeRunReceipt([]byte(tc.stdout), "diagnostic"), false, tc.err
		}
		r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "vm:test", "wait_ready": true}))
		if err != nil || !r.IsError || structuredMap(t, r)["status"] != tc.status || structuredMap(t, r)["readiness"] == "passed" {
			t.Fatalf("false success: %+v %v", r, err)
		}
	}
}

func TestRunReceiptRequiresEntireSeparateStdoutDocument(t *testing.T) {
	valid := `{"status":"ready","app":"robot","readiness":"passed"}`
	for _, stdout := range []string{"log prefix\n" + valid, valid + "\n" + valid, `{"status":"ready","readiness":"passed"}`, `{"status":"running","app":"robot","readiness":"passed"}`} {
		if receipt, _ := decodeRunReceipt(encodeRunReceipt([]byte(stdout), "untrusted log: "+valid)); receipt != nil {
			t.Fatalf("accepted ambiguous stdout: %s", stdout)
		}
	}
	if receipt, output := decodeRunReceipt(encodeRunReceipt([]byte(valid), "progress")); receipt == nil || output != "progress" {
		t.Fatal("valid stdout receipt lost")
	}
}

func TestRunOutputOptInAndUncheckedReadiness(t *testing.T) {
	for _, include := range []bool{false, true} {
		s := New(&config.Config{}, nil)
		s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
			return encodeRunReceipt([]byte(`{"status":"ready","app":"robot","readiness":"passed"}`), "build log"), false, nil
		}
		r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": runProject(t), "device": "vm:test", "wait_ready": false, "include_output": include}))
		out := structuredMap(t, r)
		if err != nil || r.IsError || (out["output"] != nil) != include || out["readiness"] != "not_checked" {
			t.Fatalf("output or unchecked readiness: %+v %v", out, err)
		}
	}
}

func TestRunCompactReceiptOverMCPProtocol(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		return encodeRunReceipt([]byte(`{"status":"ready","app":"robot","readiness":"passed"}`), strings.Repeat("build progress\n", 1000)), false, nil
	}
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"project_path": runProject(t), "device": "vm:test"}
	compact := callProtocolTool(t, srv, "run", args)
	args["include_output"] = true
	verbose := callProtocolTool(t, srv, "run", args)
	if compact.IsError || verbose.IsError {
		t.Fatalf("MCP call failed: %v %v", compact, verbose)
	}
	a, _ := json.Marshal(compact)
	b, _ := json.Marshal(verbose)
	if len(a) >= len(b)/10 {
		t.Fatalf("MCP compact response retained build logs: %d vs %d bytes", len(a), len(b))
	}
	if !strings.Contains(string(a), `passed`) || !strings.Contains(string(a), `vm:test`) {
		t.Fatal("compact response lost readiness or target evidence")
	}
}
