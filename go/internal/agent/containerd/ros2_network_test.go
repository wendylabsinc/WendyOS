package containerd

import (
	"testing"

	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestROS2UsesHostNetwork(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *runtimespec.Spec
		want bool
	}{
		{name: "missing spec"},
		{name: "missing Linux configuration", spec: &runtimespec.Spec{}},
		{name: "host namespace", spec: &runtimespec.Spec{Linux: &runtimespec.Linux{}}, want: true},
		{
			name: "host network with other namespaces",
			spec: &runtimespec.Spec{Linux: &runtimespec.Linux{Namespaces: []runtimespec.LinuxNamespace{
				{Type: runtimespec.PIDNamespace},
				{Type: runtimespec.MountNamespace},
			}}},
			want: true,
		},
		{
			name: "private network",
			spec: &runtimespec.Spec{Linux: &runtimespec.Linux{Namespaces: []runtimespec.LinuxNamespace{
				{Type: runtimespec.NetworkNamespace},
			}}},
		},
		{
			name: "joined network",
			spec: &runtimespec.Spec{Linux: &runtimespec.Linux{Namespaces: []runtimespec.LinuxNamespace{
				{Type: runtimespec.NetworkNamespace, Path: "/run/wendy/netns/app"},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ros2UsesHostNetwork(tc.spec); got != tc.want {
				t.Fatalf("ros2UsesHostNetwork() = %v, want %v", got, tc.want)
			}
		})
	}
}
