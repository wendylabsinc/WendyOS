package commands

import (
	"context"
	"testing"
)

func TestHostedMCPSettingsRequireCanonicalOrganization(t *testing.T) {
	for _, operation := range []string{"status", "enable", "disable"} {
		command := newCloudMCPCmd()
		command.SetArgs([]string{operation, "--organization", "not-an-org"})
		if err := command.ExecuteContext(context.Background()); err == nil {
			t.Fatal("invalid organization accepted")
		}
	}
}
func TestHostedMCPTunnelRejectsPublicListen(t *testing.T) {
	command := newCloudMCPCmd()
	command.SetArgs([]string{"tunnel", "--device", "mcp://mcp.wendy.dev/orgs/11111111-1111-4111-8111-111111111111/devices/22222222-2222-4222-8222-222222222222", "--listen", "0.0.0.0:2222"})
	if err := command.ExecuteContext(context.Background()); err == nil {
		t.Fatal("public forward accepted")
	}
}
