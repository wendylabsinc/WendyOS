package commands

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// TestServiceBuildSpecRepositoryMatchesDeployedImage is the contract that makes
// a remote multi-service build a deploy rather than a build.
//
// The build host pushes to PushTarget.Repository; runMultiServiceWithAgent then
// creates the container from localhost:<port>/<app>-<service>:latest. Nothing
// checks that those two agree at run time — a mismatch builds successfully,
// delivers an image nobody reads, and starts the container from whatever older
// image still holds the expected name. So they are asserted to agree here,
// against the literal format createService uses.
func TestServiceBuildSpecRepositoryMatchesDeployedImage(t *testing.T) {
	const (
		appID   = "CokeDetector"
		service = "Inference"
		regPort = 5000
	)
	base := &agentpbv2.PushTarget{AssetId: 41, RegistryPort: regPort, AgentPort: 50052}

	spec := serviceBuildSpec(base, serviceImageRepo(appID, service), "linux/arm64", "Dockerfile.generated", nil, chunkingAuto, nil)

	deployed := fmt.Sprintf("localhost:%d/%s-%s:latest", regPort,
		strings.ToLower(appID), strings.ToLower(service))
	pushed := fmt.Sprintf("localhost:%d/%s", spec.GetPushTarget().GetRegistryPort(), spec.GetPushTarget().GetRepository())
	if pushed != deployed {
		t.Fatalf("build host pushes %q but the deploy creates from %q", pushed, deployed)
	}
}

// The push target's identity must be copied from the one resolved target, not
// re-derived: a wrong asset id delivers someone else's device, and a zero agent
// port makes the build host fall back to its own configured default.
func TestServiceBuildSpecCarriesTargetIdentity(t *testing.T) {
	base := &agentpbv2.PushTarget{AssetId: 7, RegistryPort: 5000, AgentPort: 50052}

	spec := serviceBuildSpec(base, "app-web", "linux/arm64", "Dockerfile", nil, chunkingAuto, nil)

	got := spec.GetPushTarget()
	if got.GetAssetId() != base.GetAssetId() || got.GetAgentPort() != base.GetAgentPort() || got.GetRegistryPort() != base.GetRegistryPort() {
		t.Fatalf("push target = %+v, want the resolved target's identity %+v", got, base)
	}
	// One service, one target: push_targets is the fleet field and an agent that
	// predates it would see an empty spec and deliver nowhere.
	if len(spec.GetPushTargets()) != 0 {
		t.Fatalf("push_targets = %v, want the single-target field only", spec.GetPushTargets())
	}
}

// Each service must get its OWN build-context directory on the build host.
// AppId is the only thing that selects it, BuildImage clears and re-extracts it
// under a lock, so a group sharing one id would serialise its services and
// re-transfer every context on every build.
func TestServiceBuildSpecScopesContextPerService(t *testing.T) {
	base := &agentpbv2.PushTarget{AssetId: 1}

	web := serviceBuildSpec(base, "app-web", "linux/arm64", "Dockerfile", nil, chunkingAuto, nil)
	worker := serviceBuildSpec(base, "app-worker", "linux/arm64", "Dockerfile", nil, chunkingAuto, nil)

	if web.GetAppId() == worker.GetAppId() {
		t.Fatalf("both services build under app id %q; each needs its own context directory", web.GetAppId())
	}
	if web.GetAppId() != "app-web" {
		t.Fatalf("app id = %q, want the per-service repo", web.GetAppId())
	}
}

// --chunking must mean the same thing for a service of a group as it does for a
// single-service app: the flag is carried to the build host, not reinterpreted.
func TestServiceBuildSpecCarriesChunkingMode(t *testing.T) {
	base := &agentpbv2.PushTarget{AssetId: 1}
	for _, mode := range []string{chunkingAuto, chunkingForce, chunkingOff, ""} {
		spec := serviceBuildSpec(base, "app-web", "linux/arm64", "Dockerfile", nil, mode, nil)
		if got, want := spec.GetChunking(), buildChunkingMode(mode); got != want {
			t.Fatalf("--chunking=%q sent as %v, want %v", mode, got, want)
		}
	}
}

// A fleet deploy of a service group must be refused rather than narrowed to the
// primary: the group lifecycle is orchestrated against one connection, so the
// extra devices would be silently dropped after a successful-looking run.
func TestRejectMultiServiceFleetRun(t *testing.T) {
	if err := rejectMultiServiceFleetRun(runOptions{}); err != nil {
		t.Fatalf("single-device run refused: %v", err)
	}
	err := rejectMultiServiceFleetRun(runOptions{fleetDevices: []string{"ccr2", "ccr3"}})
	if err == nil {
		t.Fatal("fleet run of a service group accepted; the extra devices would be dropped")
	}
	for _, name := range []string{"ccr2", "ccr3"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("got %v, want the dropped device %s named", err, name)
		}
	}
}

// appRepository is what a single-service app's remote build pushes to, and it
// must keep matching localRegistryReference's <app>:latest — the same
// build-pushes-here/deploy-reads-there pair asserted above, for the path that
// already worked.
func TestAppRepositoryIsLowercasedAppLatest(t *testing.T) {
	if got, want := appRepository(&appconfig.AppConfig{AppID: "MyApp"}), "myapp:latest"; got != want {
		t.Fatalf("appRepository = %q, want %q", got, want)
	}
}
