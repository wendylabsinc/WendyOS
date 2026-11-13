package commands

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func stubHostPorts(t *testing.T, holder string, answers, checked bool) *int {
	t.Helper()
	origHolder, origAnswers, origListens := hostPortHoldersFn, hostPortAnswersFn, appListensOnPortFn
	t.Cleanup(func() {
		hostPortHoldersFn, hostPortAnswersFn, appListensOnPortFn = origHolder, origAnswers, origListens
	})
	appListensOnPortFn = func(context.Context, *grpcclient.AgentConnection, string, int) bool { return false }
	lookups := 0
	hostPortHoldersFn = func(context.Context, *grpcclient.AgentConnection, string, int) []string {
		lookups++
		if holder == "" {
			return nil
		}
		return strings.Split(holder, ",")
	}
	hostPortAnswersFn = func(context.Context, *grpcclient.AgentConnection, *appconfig.AppConfig, int) (bool, bool) {
		return answers, checked
	}
	return &lookups
}

func hostWebApp() (*grpcclient.AgentConnection, *appconfig.AppConfig) {
	conn := &grpcclient.AgentConnection{ContainerService: &unavailableContainerClient{}}
	cfg := appUsing(appconfig.EntitlementNetwork)
	cfg.Entitlements = append(cfg.Entitlements, appconfig.Entitlement{Type: appconfig.EntitlementHTTP, Port: 8000})
	return conn, cfg
}

func TestPreflightHostPortsRefusesAConfirmedClash(t *testing.T) {
	stubHostPorts(t, "other.app", true, true)
	conn, cfg := hostWebApp()
	err := preflightHostPorts(context.Background(), conn, cfg)
	if err == nil || !strings.Contains(err.Error(), "port 8000") || !strings.Contains(err.Error(), "other.app") {
		t.Fatalf("err = %v, want a refusal naming the port and the app", err)
	}
	if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, "wendy device apps stop other.app") {
		t.Fatalf("next steps = %q, want the stop command", steps)
	}
}

func TestPreflightHostPortsOnlyRefusesWhatItCanConfirm(t *testing.T) {
	conn, cfg := hostWebApp()
	// The other app's port does not answer on the device's address: private namespace.
	stubHostPorts(t, "other.app", false, true)
	if err := preflightHostPorts(context.Background(), conn, cfg); err != nil {
		t.Fatalf("unconfirmed clash refused: %v", err)
	}
	// No route from this machine (Cloud): a notice, and the agent decides at start.
	stubHostPorts(t, "other.app", false, false)
	var err error
	out := captureStderr(t, func() { err = preflightHostPorts(context.Background(), conn, cfg) })
	if err != nil || !strings.Contains(out, "other.app also listens on port 8000") {
		t.Fatalf("err = %v, output %q; want a notice only", err, out)
	}
}

func TestPreflightHostPortsSkipsAppsOffTheHostNetwork(t *testing.T) {
	lookups := stubHostPorts(t, "other.app", true, true)
	conn, cfg := hostWebApp()
	cfg.Entitlements[0].Mode = "mesh"
	if err := preflightHostPorts(context.Background(), conn, cfg); err != nil || *lookups != 0 {
		t.Fatalf("err = %v, lookups = %d; want no check for a mesh app", err, *lookups)
	}
}

func TestPreflightHostPortsIgnoresTheSketchOwnerAboutToBeStopped(t *testing.T) {
	stubHostPorts(t, "other.app", true, true)
	conn, cfg := hostWebApp()
	cfg.MCU = &appconfig.MCUConfig{Sketch: "sketch"}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Featureset: []string{"mcu-flash:zephyr-1", "mcu-sketch-owner:other.app"}})
	if err := preflightHostPorts(context.Background(), conn, cfg); err != nil {
		t.Fatalf("refused over the app this deploy stops: %v", err)
	}
	// The owner counts even when an app in its own namespace is listed first.
	stubHostPorts(t, "isolated.app,other.app", true, true)
	if err := preflightHostPorts(context.Background(), conn, cfg); err != nil {
		t.Fatalf("refused although the sketch owner holds the port: %v", err)
	}
	stubHostPorts(t, "other.app", true, true)
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Featureset: []string{"mcu-flash:zephyr-1", "mcu-sketch-owner:third.app"}})
	if err := preflightHostPorts(context.Background(), conn, cfg); err == nil {
		t.Fatal("a clash with an app that keeps running was not refused")
	}
}

func TestDeviceRefusalOrShowsTheDevicesRefusal(t *testing.T) {
	refusal := status.Error(codes.FailedPrecondition, "port 8000 is already used by app other.app; stop it or change this app's port in wendy.json")
	for _, what := range []string{"creating container", "waiting for container start", "starting container"} {
		err := deviceRefusalOr(refusal, what)
		if ErrorClass(err) != "project_target_mismatch" || strings.Contains(err.Error(), "rpc error") || !strings.Contains(err.Error(), "port 8000 is already used by app other.app") {
			t.Fatalf("%s: err = %v (class %q), want the plain refusal", what, err, ErrorClass(err))
		}
	}
	if other := deviceRefusalOr(status.Error(codes.Internal, "boom"), "creating container"); !strings.HasPrefix(other.Error(), "creating container: ") {
		t.Fatalf("other failures changed: %v", other)
	}
}

func TestPreflightHostPortsPassesARedeployOfTheAppServingThePort(t *testing.T) {
	// Another app listens on the same port number in its own namespace, and
	// the port answers because this app's running version serves it.
	stubHostPorts(t, "isolated.app", true, true)
	appListensOnPortFn = func(_ context.Context, _ *grpcclient.AgentConnection, appID string, _ int) bool {
		return appID == "app"
	}
	conn, cfg := hostWebApp()
	if err := preflightHostPorts(context.Background(), conn, cfg); err != nil {
		t.Fatalf("a redeploy of the app that serves the port was refused: %v", err)
	}
}
