package containerd

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"go.uber.org/zap"
	"path/filepath"
	"testing"
)

func TestNvidiaCDIFallbackOnlyForMissingName(t *testing.T) {
	for _, first := range []string{"all", "gpu0"} {
		t.Run(first, func(t *testing.T) {
			client := &Client{logger: zap.NewNop()}
			spec := oci.DefaultSpec("/rootfs", nil)
			initialMounts := len(spec.Mounts)
			broken := cdi.CDIDevice{Name: "all", ContainerEdits: cdi.CDIContainerEdits{
				DeviceNodes: []cdi.CDIDeviceNode{{Path: filepath.Join(t.TempDir(), "absent")}},
			}}
			devices := []cdi.CDIDevice{broken}
			if first != "all" {
				devices = append([]cdi.CDIDevice{{Name: first}}, devices...)
			}
			input := &cdi.CDISpecification{Devices: devices, ContainerEdits: &cdi.CDIContainerEdits{
				Mounts: []cdi.CDIMount{{HostPath: "/driver", ContainerPath: "/driver"}},
				Hooks:  []cdi.CDIHook{{HookName: "createContainer", Path: "/hook"}},
			}}
			err := client.applyNvidiaCDISpec(spec, input, "fixture.yaml")
			if !errors.Is(err, cdi.ErrDevicesUnresolved) {
				t.Fatalf("lost unresolved error: %v", err)
			}
			if len(spec.Mounts) != initialMounts+1 || len(spec.Hooks.CreateContainer) != 1 {
				t.Fatal("partial edits applied more than once")
			}
		})
	}
}

func TestNvidiaCDIFallbackSelectsFirstWhenAllAbsent(t *testing.T) {
	client := &Client{logger: zap.NewNop()}
	spec := oci.DefaultSpec("/rootfs", nil)
	input := &cdi.CDISpecification{Devices: []cdi.CDIDevice{{Name: "gpu0", ContainerEdits: cdi.CDIContainerEdits{Env: []string{"CDI_FALLBACK=ok"}}}}}
	if err := client.applyNvidiaCDISpec(spec, input, "fixture.yaml"); err != nil {
		t.Fatal(err)
	}
	if spec.Process.Env[len(spec.Process.Env)-1] != "CDI_FALLBACK=ok" {
		t.Fatal("fallback not applied")
	}
}
