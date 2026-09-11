package solve

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// stubLookups replaces the two seams Address reads the world through, and
// restores them when the test ends.
//
// The environment is a map rather than os.Setenv because a real environment
// variable is process-global: it would leak into every other test in this
// binary, and on a machine where the developer already has BUILDKIT_HOST set
// the results would depend on the shell the suite was run from. The stubs are
// themselves package-level variables, so no test here may call t.Parallel() —
// two cases installing different environments would race on them.
func stubLookups(t *testing.T, env map[string]string, dockerOnPath bool) {
	t.Helper()
	prevEnv, prevPath := lookupEnv, lookPath
	t.Cleanup(func() { lookupEnv, lookPath = prevEnv, prevPath })

	lookupEnv = func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	lookPath = func(file string) (string, error) {
		if file == "docker" && dockerOnPath {
			return "/usr/bin/docker", nil
		}
		return "", exec.ErrNotFound
	}
}

func TestAddress(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		dockerOnPath bool
		want         string
	}{
		{
			name:         "BUILDKIT_HOST wins over every other signal",
			env:          map[string]string{"BUILDKIT_HOST": "tcp://10.0.0.2:1234", "WENDY_AGENT_SOCKET": "/run/wendy.sock"},
			dockerOnPath: false,
			want:         "tcp://10.0.0.2:1234",
		},
		{
			name:         "a blank BUILDKIT_HOST is not a choice",
			env:          map[string]string{"BUILDKIT_HOST": "  "},
			dockerOnPath: true,
			want:         "docker-container://buildx_buildkit_wendy0",
		},
		{
			name:         "on-device without docker uses the device socket",
			env:          map[string]string{"WENDY_AGENT_SOCKET": "/run/wendy.sock"},
			dockerOnPath: false,
			want:         DeviceAddress,
		},
		{
			name:         "on-device with docker present keeps the buildx daemon",
			env:          map[string]string{"WENDY_AGENT_SOCKET": "/run/wendy.sock"},
			dockerOnPath: true,
			want:         "docker-container://buildx_buildkit_wendy0",
		},
		{
			name:         "off-device falls back to the buildx daemon",
			env:          map[string]string{},
			dockerOnPath: true,
			want:         "docker-container://buildx_buildkit_wendy0",
		},
		{
			name:         "the buildx builder name is overridable",
			env:          map[string]string{"WENDY_BUILDX_BUILDER": "ci"},
			dockerOnPath: true,
			want:         "docker-container://buildx_buildkit_ci0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubLookups(t, tc.env, tc.dockerOnPath)
			got, err := Address(context.Background())
			if err != nil {
				t.Fatalf("Address: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Address = %q, want %q", got, tc.want)
			}
		})
	}
}

// Off-device with no docker there is nothing to connect to: the buildx daemon
// lives inside a container reached by `docker exec`. Returning the address
// anyway would defer the failure to a dial error that names a container the
// user has never heard of.
func TestAddressWithoutDockerOffDeviceFails(t *testing.T) {
	stubLookups(t, map[string]string{}, false)
	got, err := Address(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got address %q", got)
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("error should name the missing dependency, got %v", err)
	}
	if !strings.Contains(err.Error(), "BUILDKIT_HOST") {
		t.Fatalf("error should point at the override, got %v", err)
	}
}
