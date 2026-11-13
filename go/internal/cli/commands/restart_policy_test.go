package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestConfiguredRestartPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		opts   runOptions
		want   agentpb.RestartPolicyMode
	}{
		{"unchanged default", "", runOptions{}, agentpb.RestartPolicyMode_DEFAULT},
		{"finite project", "no", runOptions{}, agentpb.RestartPolicyMode_NO},
		{"retry failures", "on-failure", runOptions{}, agentpb.RestartPolicyMode_ON_FAILURE},
		{"service", "unless-stopped", runOptions{}, agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{"explicit no", "unless-stopped", runOptions{noRestart: true}, agentpb.RestartPolicyMode_NO},
		{"explicit retry", "no", runOptions{restartOnFailure: true}, agentpb.RestartPolicyMode_ON_FAILURE},
		{"explicit service", "no", runOptions{restartUnlessStopped: true}, agentpb.RestartPolicyMode_UNLESS_STOPPED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRestartPolicy(tc.opts.withConfiguredRestartPolicy(tc.policy)).GetMode(); got != tc.want {
				t.Fatalf("restart mode = %v, want %v", got, tc.want)
			}
		})
	}
}
