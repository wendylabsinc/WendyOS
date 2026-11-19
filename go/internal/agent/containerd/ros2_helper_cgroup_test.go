package containerd

import (
	"testing"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

func TestPlaceROS2Helper(t *testing.T) {
	const name = "wendy-ros2-host-inspector"
	t.Run("unified sets the budgeted path", func(t *testing.T) {
		spec := &localoci.Spec{Linux: &localoci.Linux{}}
		placeROS2Helper(spec, name, true)
		if got := spec.Linux.CgroupsPath; got != "/wendy-apps/"+name {
			t.Fatalf("CgroupsPath = %q", got)
		}
	})
	t.Run("not unified leaves the path empty", func(t *testing.T) {
		spec := &localoci.Spec{Linux: &localoci.Linux{}}
		placeROS2Helper(spec, name, false)
		if spec.Linux.CgroupsPath != "" {
			t.Fatalf("CgroupsPath = %q", spec.Linux.CgroupsPath)
		}
	})
	t.Run("nil Linux is created when placing", func(t *testing.T) {
		spec := &localoci.Spec{}
		placeROS2Helper(spec, name, true)
		if spec.Linux == nil || spec.Linux.CgroupsPath != "/wendy-apps/"+name {
			t.Fatalf("Linux = %+v", spec.Linux)
		}
	})
	t.Run("nil Linux stays nil when not placing", func(t *testing.T) {
		spec := &localoci.Spec{}
		placeROS2Helper(spec, name, false)
		if spec.Linux != nil {
			t.Fatalf("Linux = %+v", spec.Linux)
		}
	})
	t.Run("existing path is overwritten only when placing", func(t *testing.T) {
		spec := &localoci.Spec{Linux: &localoci.Linux{CgroupsPath: "/custom"}}
		placeROS2Helper(spec, name, false)
		if spec.Linux.CgroupsPath != "/custom" {
			t.Fatalf("untouched case changed to %q", spec.Linux.CgroupsPath)
		}
		placeROS2Helper(spec, name, true)
		if spec.Linux.CgroupsPath != "/wendy-apps/"+name {
			t.Fatalf("placing case = %q", spec.Linux.CgroupsPath)
		}
	})
	t.Run("unsafe name leaves the spec untouched", func(t *testing.T) {
		spec := &localoci.Spec{}
		placeROS2Helper(spec, "a/b", true)
		if spec.Linux != nil {
			t.Fatalf("Linux = %+v", spec.Linux)
		}
	})
}
