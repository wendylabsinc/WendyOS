package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The MCP run tool (internal/cli/mcp) sets WENDY_RUN_NO_CLOUD_FALLBACK=1 for
// the `wendy run` it spawns so a failed direct connect cannot become a deploy
// to a same-named cloud device.
func TestCloudFallbackDisabledOnlyByMCPRunEnvironment(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		t.Setenv("WENDY_RUN_NO_CLOUD_FALLBACK", value)
		if got := cloudFallbackDisabled(""); got != want {
			t.Errorf("WENDY_RUN_NO_CLOUD_FALLBACK=%q: disabled = %v, want %v", value, got, want)
		}
	}
}

// The variable pins only the deploy target (cloudName == ""). A build host or
// fleet member named explicitly still reaches the cloud tunnel, so a
// cloud-only build host keeps working under the MCP run tool.
func TestMCPRunKeepsCloudFallbackForBuildHostsOnly(t *testing.T) {
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443"}}})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	const unreachable = "127.0.0.1:1"
	oldFlag, oldConnect := deviceFlag, cloudFallbackConnectFn
	t.Cleanup(func() { deviceFlag, cloudFallbackConnectFn = oldFlag, oldConnect })
	deviceFlag = unreachable
	var tunnelled []string
	cloudFallbackConnectFn = func(_ context.Context, _, name, _ string, _ *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		tunnelled = append(tunnelled, name)
		return nil, errors.New("offline")
	}
	for _, env := range []string{"", "1"} {
		t.Setenv("WENDY_RUN_NO_CLOUD_FALLBACK", env)
		tunnelled = nil
		for _, cloudName := range []string{"", "build-host"} {
			if _, err := resolveWithCloudFallback(context.Background(), cloudName, SelectDevice(unreachable), NonInteractive(), SuppressUpdateCheck()); err == nil {
				t.Fatalf("env=%q cloudName=%q: unreachable device resolved", env, cloudName)
			}
		}
		want := []string{unreachable, "build-host"}
		if env == "1" {
			want = []string{"build-host"}
		}
		if len(tunnelled) != len(want) || tunnelled[0] != want[0] || tunnelled[len(tunnelled)-1] != want[len(want)-1] {
			t.Errorf("env=%q: tunnelled to %v, want %v", env, tunnelled, want)
		}
	}
}
