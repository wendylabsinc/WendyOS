package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRunPreservesTargetAndReportsReadinessSeparately(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "wendy.json"), []byte(`{"appId":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		target     commandTarget
		args       map[string]any
		prefix     []string
		wantTarget string
	}{
		{"LAN", commandTarget{Device: "robot.local:50051", Transport: "direct"}, nil, []string{"run"}, "robot.local:50051"},
		{"VM", commandTarget{Device: "vm:go2", Transport: "direct"}, nil, []string{"run"}, "vm:go2"},
		{"cloud session", commandTarget{Device: "robot", Transport: "cloud", CloudGRPC: "org.example:443", BrokerURL: "relay.example:443"}, nil, []string{"cloud", "run"}, "robot"},
		{"cloud identity", commandTarget{Device: "robot", Transport: "cloud", CloudGRPC: "org.example:443", Selector: "cloud://org.example:443/org/7/asset/42"}, nil, []string{"run"}, "cloud://org.example:443/org/7/asset/42"},
		{"explicit beats session", commandTarget{Device: "old-robot", Transport: "cloud"}, map[string]any{"device": "vm:test"}, []string{"run"}, "vm:test"},
		{"cloud without session", commandTarget{}, map[string]any{"device_name": "robot", "cloud_grpc": "org.example:443"}, []string{"cloud", "run"}, "robot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(&config.Config{DefaultDevice: "wrong-default"}, nil)
			s.commandTarget = tc.target
			var actual []string
			s.runCommandFn = func(_ context.Context, args []string, _ commandTarget, _ int) (string, bool, error) {
				actual = args
				return "container started", false, nil
			}
			args := map[string]any{"project_path": project}
			for k, v := range tc.args {
				args[k] = v
			}
			r, err := s.handleRun(context.Background(), callToolReq("run", args))
			if err != nil || r.IsError {
				t.Fatalf("run failed: %v %v", err, r)
			}
			if !reflect.DeepEqual(actual[:len(tc.prefix)], tc.prefix) {
				t.Fatalf("wrong transport: %v", actual)
			}
			joined := strings.Join(actual, " ")
			if !strings.Contains(joined, "--device "+tc.wantTarget) || !strings.Contains(joined, "--detach") {
				t.Fatalf("wrong target/flags: %v", actual)
			}
			if tc.name == "cloud session" && (!strings.Contains(joined, "--cloud-grpc org.example:443") || !strings.Contains(joined, "--broker-url relay.example:443")) {
				t.Fatalf("lost cloud scope: %v", actual)
			}
			result := structuredMap(t, r)
			if result["readiness"] != "not_checked" || result["status"] != "started" {
				t.Fatalf("wrong status: %v", result)
			}
		})
	}
}

func TestRunRejectsAmbiguousTargetsAndNeverUsesDefault(t *testing.T) {
	s := New(&config.Config{DefaultDevice: "wrong"}, nil)
	for _, args := range []map[string]any{nil, {"device": "local", "device_name": "cloud"}, {"device": "local", "cloud_grpc": "other:443"}, {"cloud_grpc": "other:443"}} {
		if _, err := s.runTarget(callToolReq("run", args)); err == nil {
			t.Fatalf("accepted ambiguous/default target %v", args)
		}
	}
	s.commandTarget = commandTarget{Device: "robot", Transport: "cloud", CloudGRPC: "first:443"}
	if _, err := s.runTarget(callToolReq("run", map[string]any{"cloud_grpc": "second:443"})); err == nil {
		t.Fatal("allowed target scope to change implicitly")
	}
}

func TestRunFailuresKeepStructuredDiagnostics(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "wendy.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, nil)
	s.runCommandFn = func(context.Context, []string, commandTarget, int) (string, bool, error) {
		return "final diagnostic", true, errors.New("exit status 1")
	}
	r, err := s.handleRun(context.Background(), callToolReq("run", map[string]any{"project_path": project, "device": "vm:test"}))
	if err != nil || !r.IsError {
		t.Fatalf("expected tool error: %v %v", r, err)
	}
	data := structuredMap(t, r)
	if data["output"] != "final diagnostic" || data["truncated"] != true || data["error_code"] != "INTERNAL" {
		t.Fatalf("lost failure diagnostics: %v", data)
	}
}

func TestRunTailBoundsMemoryAndPreservesFinalDiagnostic(t *testing.T) {
	w := &runTail{limit: 32}
	_, _ = w.Write([]byte(strings.Repeat("build output", 1000)))
	_, _ = w.Write([]byte("\nFINAL ERROR"))
	if len(w.data) > 32 || !w.truncated || !strings.HasSuffix(string(w.data), "FINAL ERROR") {
		t.Fatalf("tail = %q truncated=%v", w.data, w.truncated)
	}
}

func TestCloudRunPinsOrganizationAndAsset(t *testing.T) {
	auth := &config.AuthConfig{CloudGRPC: "shared.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	target := cloudCommandTarget(auth, mcpCloudDevice{name: "same-name", legacyID: 42}, "selected-broker:443")
	if target.Selector != "cloud://shared.example:443/org/7/asset/42" {
		t.Fatalf("lost cloud identity: %+v", target)
	}
	got := runEnvironment([]string{"PATH=/bin", "WENDY_BROKER_URL=wrong:443"}, target)
	if !reflect.DeepEqual(got, []string{"PATH=/bin", "WENDY_BROKER_URL=selected-broker:443"}) {
		t.Fatalf("lost broker routing: %v", got)
	}
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/test"
	target = cloudCommandTarget(auth, mcpCloudDevice{name: "same-name", key: "22222222-2222-4222-8222-222222222222", isV2: true}, "")
	if target.Selector != "cloud://shared.example:443/tenant/11111111-1111-4111-8111-111111111111/asset/22222222-2222-4222-8222-222222222222" {
		t.Fatalf("lost tenant identity: %+v", target)
	}
}

func TestRunStartAndLegacyDeploy(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "wendy.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args             map[string]any
		created, invalid bool
	}{
		{nil, false, false}, {map[string]any{"start": false}, true, false},
		{map[string]any{"deploy": true}, true, false}, {map[string]any{"start": false, "deploy": true}, true, false},
		{map[string]any{"start": true, "deploy": true}, false, true}, {map[string]any{"start": "no"}, false, true},
	} {
		s := New(&config.Config{}, nil)
		called := false
		s.runCommandFn = func(_ context.Context, args []string, _ commandTarget, limit int) (string, bool, error) {
			called = true
			if strings.Contains(strings.Join(args, " "), "--deploy") != tc.created || limit != 16384 {
				t.Fatalf("wrong flags/budget: %v %d", args, limit)
			}
			return "ok", false, nil
		}
		args := map[string]any{"project_path": project, "device": "vm:test"}
		for k, v := range tc.args {
			args[k] = v
		}
		r, err := s.handleRun(context.Background(), callToolReq("run", args))
		if err != nil || r.IsError != tc.invalid || called == tc.invalid {
			t.Fatalf("args %v: %v %v", args, r, err)
		}
		if !tc.invalid && (structuredMap(t, r)["status"] == "created") != tc.created {
			t.Fatalf("incorrect deployment status: %v", r)
		}
	}
}
