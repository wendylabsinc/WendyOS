package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

var nvidiaCTKMountOptions = []string{"ro", "nosuid", "nodev", "rbind", "rprivate"}

// storedGPUSpec is the record an older agent left for a GPU app deployed on
// the previous OS: the driver files of that OS, no note of where they came
// from, and one field the agent's own spec type does not model.
func storedGPUSpec(t *testing.T) []byte {
	t.Helper()
	spec := localoci.DefaultSpec("/rootfs", nil)
	spec.Mounts = append(spec.Mounts,
		localoci.Mount{
			Destination: "/usr/lib/libgstrtspserver-1.0.so.0.2805.0", Source: "/usr/lib/libgstrtspserver-1.0.so.0.2805.0",
			Type: "bind", Options: nvidiaCTKMountOptions,
		},
		localoci.Mount{Destination: "/dev/dri", Source: "/dev/dri", Type: "bind", Options: []string{"rbind", "rw", "nosuid", "noexec"}},
	)
	spec.Hooks = &localoci.Hooks{CreateContainer: []localoci.Hook{{
		Path: "/usr/bin/nvidia-ctk",
		Args: []string{"nvidia-ctk", "hook", "create-symlinks", "--link", "libcublas.so.13.4.0.1::/usr/lib/libcublas.so.13"},
	}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["domainname"] = json.RawMessage(`"kept"`)
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// currentNvidiaCDI stands in for the list the updated OS generated at boot.
func currentNvidiaCDI(t *testing.T) {
	t.Helper()
	previous := loadNvidiaCDISpec
	t.Cleanup(func() { loadNvidiaCDISpec = previous })
	loadNvidiaCDISpec = func() (*cdi.CDISpecification, string, error) {
		return &cdi.CDISpecification{Devices: []cdi.CDIDevice{{Name: "all", ContainerEdits: cdi.CDIContainerEdits{
			Mounts: []cdi.CDIMount{{
				HostPath: "/usr/lib/libcublas.so.13.4.1.3", ContainerPath: "/usr/lib/libcublas.so.13.4.1.3",
				Options: nvidiaCTKMountOptions,
			}},
			Hooks: []cdi.CDIHook{{
				HookName: "createContainer", Path: "/usr/bin/nvidia-ctk",
				Args: []string{"nvidia-ctk", "hook", "create-symlinks", "--link", "libcublas.so.13.4.1.3::/usr/lib/libcublas.so.13"},
			}},
		}}}}, "/etc/cdi/nvidia.yaml", nil
	}
}

// storedContainer loads store's record through a containerd client, the way
// the start path holds it.
func storedContainer(t *testing.T, store *deviceSpecStore, logger *zap.Logger) (*Client, containerd.Container) {
	t.Helper()
	remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := remote.LoadContainer(context.Background(), store.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{client: remote, logger: logger}, ctr
}

var gpuLabels = map[string]string{"sh.wendy/entitlement.gpu": ""}

// The crash loop this exists for: after an OS update the stored spec names a
// driver file the new image does not have, and the runtime refuses to create
// the container. The start must leave a spec the runtime can create.
func TestCDIRefresh_RepairsTheStoredSpecAfterAnOSUpdate(t *testing.T) {
	currentNvidiaCDI(t)
	original := storedGPUSpec(t)
	store := &deviceSpecStore{record: containers.Container{
		ID: "gpu-probe", Spec: &anypb.Any{TypeUrl: "test-spec", Value: original},
		SnapshotKey: "keep-snapshot", Labels: map[string]string{"other": "keep"},
	}}
	client, ctr := storedContainer(t, store, zap.NewNop())

	client.refreshNvidiaCDIForStart(context.Background(), ctr, "gpu-probe", gpuLabels)

	var saved localoci.Spec
	if err := json.Unmarshal(store.record.Spec.GetValue(), &saved); err != nil {
		t.Fatal(err)
	}
	var dests []string
	for _, m := range saved.Mounts {
		dests = append(dests, m.Destination)
	}
	if slices.Contains(dests, "/usr/lib/libgstrtspserver-1.0.so.0.2805.0") {
		t.Error("the file the new OS does not have is still mounted")
	}
	for _, want := range []string{"/usr/lib/libcublas.so.13.4.1.3", "/dev/dri", "/proc"} {
		if !slices.Contains(dests, want) {
			t.Errorf("%s is not mounted", want)
		}
	}
	if saved.Hooks == nil || len(saved.Hooks.CreateContainer) != 1 ||
		saved.Hooks.CreateContainer[0].Args[4] != "libcublas.so.13.4.1.3::/usr/lib/libcublas.so.13" {
		t.Errorf("hooks were not brought up to date: %+v", saved.Hooks)
	}

	// The write is confined to the spec, and to the parts of it that changed.
	if !reflect.DeepEqual(store.masks, []string{"spec"}) {
		t.Errorf("update masks = %v; want only the spec", store.masks)
	}
	if store.record.SnapshotKey != "keep-snapshot" || store.record.Labels["other"] != "keep" {
		t.Error("the container's snapshot or labels were disturbed")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(store.record.Spec.GetValue(), &document); err != nil {
		t.Fatal(err)
	}
	if string(document["domainname"]) != `"kept"` {
		t.Errorf("a field the agent does not model was dropped: %s", document["domainname"])
	}
	var before map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if string(document["linux"]) != string(before["linux"]) {
		t.Error("the linux section changed although the list names no devices")
	}
}

// Only apps that were given the driver files have any to refresh.
func TestCDIRefresh_LeavesOtherContainersAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		load   func() (*cdi.CDISpecification, string, error)
	}{
		"no gpu entitlement": {labels: map[string]string{"sh.wendy/entitlement.camera": ""}},
		"host has no list": {labels: gpuLabels, load: func() (*cdi.CDISpecification, string, error) {
			return nil, "", &cdi.CDIError{Message: "CDI spec not found"}
		}},
		"list names no device": {labels: gpuLabels, load: func() (*cdi.CDISpecification, string, error) {
			return &cdi.CDISpecification{}, "/etc/cdi/nvidia.yaml", nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			currentNvidiaCDI(t)
			if tc.load != nil {
				loadNvidiaCDISpec = tc.load
			}
			store := &deviceSpecStore{record: containers.Container{ID: "app", Spec: &anypb.Any{TypeUrl: "test-spec", Value: storedGPUSpec(t)}}}
			client, ctr := storedContainer(t, store, zap.NewNop())

			client.refreshNvidiaCDIForStart(context.Background(), ctr, "app", tc.labels)

			if store.masks != nil {
				t.Errorf("the stored spec was rewritten (masks %v)", store.masks)
			}
		})
	}
}

// The refresh is a repair, not a gate. If it cannot be saved the start goes
// ahead with the stored spec and the runtime has the last word, as before.
func TestCDIRefresh_AFailedWriteIsReportedAndDoesNotStopTheStart(t *testing.T) {
	currentNvidiaCDI(t)
	original := storedGPUSpec(t)
	store := &deviceSpecStore{
		record:    containers.Container{ID: "gpu-probe", Spec: &anypb.Any{TypeUrl: "test-spec", Value: original}},
		updateErr: errors.New("disk full"),
	}
	core, logs := observer.New(zapcore.WarnLevel)
	client, ctr := storedContainer(t, store, zap.New(core))

	client.refreshNvidiaCDIForStart(context.Background(), ctr, "gpu-probe", gpuLabels)

	if string(store.record.Spec.GetValue()) != string(original) {
		t.Error("the stored spec changed although the write failed")
	}
	if logs.Len() != 1 || logs.All()[0].ContextMap()["app_name"] != "gpu-probe" {
		t.Errorf("want one warning naming the app, got %v", logs.All())
	}
}

// The create path applies the device named "all" and falls back to the first
// one. The refresh has to pick the same device, or it would swap one device's
// files for another's.
func TestNvidiaCDIDeviceName(t *testing.T) {
	for _, tc := range []struct {
		devices []string
		want    string
		ok      bool
	}{
		{[]string{"all"}, "all", true},
		{[]string{"gpu0", "all"}, "all", true},
		{[]string{"gpu0", "gpu1"}, "gpu0", true},
		{nil, "", false},
	} {
		spec := &cdi.CDISpecification{}
		for _, name := range tc.devices {
			spec.Devices = append(spec.Devices, cdi.CDIDevice{Name: name})
		}
		if got, ok := nvidiaCDIDeviceName(spec); got != tc.want || ok != tc.ok {
			t.Errorf("devices %v: got %q, %v; want %q, %v", tc.devices, got, ok, tc.want, tc.ok)
		}
	}
}

// startPathStore adds the listing StartContainer uses to resolve an app name.
type startPathStore struct{ *deviceSpecStore }

func (s startPathStore) List(context.Context, ...string) ([]containers.Container, error) {
	return []containers.Container{s.record}, nil
}

// Boot, the restart monitor and `apps start` all reach a container through
// StartContainer, so that is where the repair has to happen, and before the
// start reads the stored spec for anything else.
func TestStartContainer_RefreshesDriverFilesBeforeUsingTheStoredSpec(t *testing.T) {
	currentNvidiaCDI(t)
	stale := "/usr/lib/libgstrtspserver-1.0.so.0.2805.0"
	spec, err := typeurl.MarshalAny(&specs.Spec{
		Process: &specs.Process{},
		Linux:   &specs.Linux{},
		Mounts:  []specs.Mount{{Destination: stale, Source: stale, Type: "bind", Options: nvidiaCTKMountOptions}},
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := wendyLabels("gpu-probe", "", "1", nil, []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}, "", nil)
	// Stops the start right after it has read the stored spec, before it
	// would need a runtime to create a task.
	labels[labelKeySerialIdentities] = "invalid-json"
	store := &deviceSpecStore{record: containers.Container{
		ID: "gpu-probe", Labels: labels,
		Spec: &anypb.Any{TypeUrl: spec.GetTypeUrl(), Value: spec.GetValue()},
	}}
	remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(startPathStore{store})))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	c := &Client{client: remote, namespace: "wendy-test", logger: zap.NewNop()}

	if _, err := c.StartContainer(context.Background(), "gpu-probe", "", nil); err == nil {
		t.Fatal("the start was expected to stop at the serial identities")
	}

	var saved specs.Spec
	if err := json.Unmarshal(store.record.Spec.GetValue(), &saved); err != nil {
		t.Fatal(err)
	}
	var dests []string
	for _, m := range saved.Mounts {
		dests = append(dests, m.Destination)
	}
	if slices.Contains(dests, stale) || !slices.Contains(dests, "/usr/lib/libcublas.so.13.4.1.3") {
		t.Errorf("the stored spec still carries the previous OS's driver files: %v", dests)
	}
}
