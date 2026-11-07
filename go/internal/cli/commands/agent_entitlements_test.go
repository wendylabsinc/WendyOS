package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func agentKnowing(types ...string) *agentpb.GetAgentVersionResponse {
	return &agentpb.GetAgentVersionResponse{Version: "2026.10.01-120000", Os: "linux", CpuArchitecture: "arm64", EntitlementTypes: types}
}

func appUsing(types ...string) *appconfig.AppConfig {
	cfg := &appconfig.AppConfig{AppID: "app", Platform: "linux"}
	for _, t := range types {
		cfg.Entitlements = append(cfg.Entitlements, appconfig.Entitlement{Type: t})
	}
	return cfg
}

func TestAgentEntitlementsRefusesATypeTheAgentDoesNotKnow(t *testing.T) {
	err := checkAgentEntitlements(agentKnowing("network", "camera"), appUsing("network", "episode-write"))
	if ErrorClass(err) != "project_target_mismatch" {
		t.Fatalf("err = %v (class %q), want a project_target_mismatch refusal", err, ErrorClass(err))
	}
	msg := err.Error()
	if !strings.Contains(msg, `"episode-write"`) || strings.Contains(msg, `"network"`) || !strings.Contains(msg, "2026.10.01-120000") {
		t.Fatalf("message should name only the unknown type and the agent version: %q", msg)
	}
	// The MCP run tool recognizes the refusal by this phrase (runRefusals in
	// package mcp).
	if !strings.HasPrefix(msg, "the device's agent does not support") {
		t.Fatalf("message no longer starts with the phrase the MCP run tool matches: %q", msg)
	}
	if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, "wendy device update") {
		t.Fatalf("next steps = %q, want the device update", steps)
	}
}

func TestAgentEntitlementsNamesEachUnknownTypeOnce(t *testing.T) {
	err := checkAgentEntitlements(agentKnowing("network"),
		appUsing("episode-write", "network"), appUsing("data-read", "episode-write"))
	if err == nil {
		t.Fatal("no refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"data-read", "episode-write"`) || strings.Count(msg, "episode-write") != 1 {
		t.Fatalf("want each unknown type once, in order: %q", msg)
	}
}

func TestAgentEntitlementsPassesWhenTheAgentKnowsEveryType(t *testing.T) {
	if err := checkAgentEntitlements(agentKnowing("network", "camera"), appUsing("camera"), appUsing()); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// Agents predating the report list nothing; they cannot be checked.
func TestAgentEntitlementsSkipsAnAgentThatDoesNotReportThem(t *testing.T) {
	if err := checkAgentEntitlements(agentKnowing(), appUsing("episode-write")); err != nil {
		t.Fatalf("refused on an agent with no report: %v", err)
	}
}

func TestRunRefusesAnEntitlementTheAgentDoesNotKnow(t *testing.T) {
	isolateFingerprintCache(t)
	fakeAllBuildDisks(t, 100<<30)
	failDockerUse(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := appUsing("camera")
	opts := runOptions{detach: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := &grpcclient.AgentConnection{ContainerService: &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED}}
	conn.CacheAgentVersion(agentKnowing("network"))

	err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts))
	if ErrorClass(err) != "project_target_mismatch" || !strings.Contains(err.Error(), `"camera"`) {
		t.Fatalf("err = %v (class %q), want a refusal naming camera", err, ErrorClass(err))
	}
}
