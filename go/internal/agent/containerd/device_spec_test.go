package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/anypb"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type deviceSpecStore struct {
	containers.Store
	record    containers.Container
	updateErr error
	masks     []string
}

// FastRPC bind mounts retain their paths while misc minors change after boot.
func TestDeviceRefresh_NPUBindMountAfterDeviceRenumber(t *testing.T) {
	major, minor, err := localoci.ResolveDeviceNode(os.DevNull, "c", true)
	if err != nil {
		t.Fatal(err)
	}
	spec := localoci.DefaultSpec("/rootfs", nil)
	spec.Mounts = append(spec.Mounts, localoci.Mount{Source: os.DevNull, Destination: "/dev/fastrpc-cdsp", Type: "bind", Options: []string{"bind", "rw"}})
	localoci.RecordPinnedDevice(spec, os.DevNull, "c", 999, 999)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: new(int64(999)), Minor: new(int64(999)), Access: "rw"})
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceSpecStore{record: containers.Container{ID: "npu-app", Spec: &anypb.Any{TypeUrl: "test-spec", Value: raw}}}
	remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := remote.LoadContainer(context.Background(), "npu-app")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{client: remote, logger: zap.NewNop()}
	if err := client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "npu-app", map[string]string{"sh.wendy/entitlement.npu": ""}); err != nil {
		t.Fatal(err)
	}
	var saved localoci.Spec
	if err := json.Unmarshal(store.record.Spec.GetValue(), &saved); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rule := range saved.Linux.Resources.Devices {
		if rule.Allow && rule.Type == "c" && rule.Major != nil && rule.Minor != nil && *rule.Major == major && *rule.Minor == minor && rule.Access == "rw" {
			found = true
		}
		if rule.Allow && rule.Type == "c" && rule.Major != nil && *rule.Major == 999 {
			t.Fatal("stale NPU grant survived restart")
		}
	}
	if !found {
		t.Fatal("NPU-only container did not receive the current device grant")
	}
	if !reflect.DeepEqual(store.masks, []string{"spec"}) {
		t.Fatalf("expected scoped spec update, got %v", store.masks)
	}
}

func (s *deviceSpecStore) Get(context.Context, string) (containers.Container, error) {
	return s.record, nil
}
func (s *deviceSpecStore) Update(_ context.Context, record containers.Container, masks ...string) (containers.Container, error) {
	s.masks = masks
	if s.updateErr != nil {
		return containers.Container{}, s.updateErr
	}
	s.record.Spec = record.Spec
	return s.record, nil
}

func TestDeviceRefresh_PersistenceThroughContainerdClient(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stale, writeFails bool
		label             string
	}{
		{"GPU repair", true, false, "gpu"},
		{"GPU required repair write fails", true, true, "gpu"},
		{"GPU optional annotation write fails", false, true, "gpu"},
		{"NPU repair", true, false, "npu"},
		{"NPU required repair write fails", true, true, "npu"},
		{"NPU optional annotation write fails", false, true, "npu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, err := localoci.ResolveDeviceNode(os.DevNull, "c", true)
			if err != nil {
				t.Fatal(err)
			}
			spec := localoci.DefaultSpec("/rootfs", nil)
			storedMajor := major
			if tc.stale {
				storedMajor = 999
			}
			spec.Linux.Devices = []localoci.LinuxDevice{{Path: os.DevNull, Type: "c", Major: storedMajor, Minor: minor}}
			spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: new(storedMajor), Minor: new(minor), Access: "rw"})
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			store := &deviceSpecStore{record: containers.Container{ID: "app", Spec: &anypb.Any{TypeUrl: "test-spec", Value: raw}, SnapshotKey: "keep-snapshot", Labels: map[string]string{"other": "keep"}}}
			writeErr := errors.New("disk full")
			if tc.writeFails {
				store.updateErr = writeErr
			}
			remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
			if err != nil {
				t.Fatal(err)
			}
			ctr, err := remote.LoadContainer(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{client: remote, logger: zap.NewNop()}
			err = client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement." + tc.label: ""})
			if tc.stale && tc.writeFails {
				if !errors.Is(err, writeErr) || classifyStartError(err) != exitReasonStartFailed {
					t.Fatalf("required repair failure hidden: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(store.masks, []string{"spec"}) {
				t.Fatalf("unscoped write: %v", store.masks)
			}
			if store.record.SnapshotKey != "keep-snapshot" || store.record.Labels["other"] != "keep" {
				t.Fatal("unrelated metadata changed")
			}
			if !tc.writeFails {
				var persisted localoci.Spec
				if err := json.Unmarshal(store.record.Spec.GetValue(), &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted.Linux.Devices[0].Major != major {
					t.Fatal("stored binding not repaired")
				}
				store.masks = nil
				if err := client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement." + tc.label: ""}); err != nil {
					t.Fatal(err)
				}
				if store.masks != nil {
					t.Fatal("unchanged spec rewritten")
				}
			}
		})
	}
}

func TestMarshalRefreshedDeviceSpec_PreservesUnmodeledFields(t *testing.T) {
	original := []byte(`{"ociVersion":"1.0.2","process":{"apparmorProfile":"keep"},"linux":{"sysctl":{"net.ipv4.ip_forward":"0"},"resources":{"memory":{"limit":9223372036854775807}}},"futureExtension":{"enabled":true}}`)
	var spec localoci.Spec
	if err := json.Unmarshal(original, &spec); err != nil {
		t.Fatal(err)
	}
	got, err := marshalRefreshedDeviceSpec(original, &spec)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	json.Unmarshal(original, &before)
	json.Unmarshal(got, &after)
	for _, key := range []string{"process", "futureExtension"} {
		if string(before[key]) != string(after[key]) {
			t.Fatalf("lost %s", key)
		}
	}
	var linux map[string]json.RawMessage
	json.Unmarshal(after["linux"], &linux)
	if string(linux["sysctl"]) != `{"net.ipv4.ip_forward":"0"}` {
		t.Fatal("lost sysctl")
	}
	var resources map[string]json.RawMessage
	json.Unmarshal(linux["resources"], &resources)
	if string(resources["memory"]) != `{"limit":9223372036854775807}` {
		t.Fatal("changed memory limit")
	}
}

func TestDeviceRefresh_UnresolvedSourceLeavesStoredSpecIntact(t *testing.T) {
	for _, tc := range []struct {
		entitlement string
		invalid     bool
	}{{"gpu", false}, {"gpu", true}, {"npu", false}, {"npu", true}} {
		t.Run(fmt.Sprintf("%s/invalid=%t", tc.entitlement, tc.invalid), func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "device")
			if tc.invalid {
				if err := os.WriteFile(source, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			spec := localoci.DefaultSpec("/rootfs", nil)
			spec.Linux.Devices = []localoci.LinuxDevice{
				{Path: os.DevNull, Type: "c", Major: 999},
				{Path: source, Type: "c", Major: 998},
			}
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			store := &deviceSpecStore{record: containers.Container{ID: "app", Spec: &anypb.Any{Value: raw}}}
			remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
			if err != nil {
				t.Fatal(err)
			}
			ctr, err := remote.LoadContainer(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{client: remote, logger: zap.NewNop()}
			err = client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement." + tc.entitlement: ""})
			if err == nil {
				t.Fatal("unresolved device accepted")
			}
			if !tc.invalid && !errors.Is(err, ErrDeviceUnavailable) {
				t.Fatalf("missing device classification: %v", err)
			}
			if len(store.masks) != 0 || !bytes.Equal(store.record.Spec.GetValue(), raw) {
				t.Fatal("failed preflight persisted partial repairs")
			}
		})
	}
}

func TestDeviceRefresh_NPUCgroupOnlyPinsAfterDeviceRenumbering(t *testing.T) {
	// FastRPC is bind-mounted: only the access rules and pin annotation carry
	// device numbers. Swap two host bindings to model their reboot reordering.
	paths := []string{os.DevNull, "/dev/zero"}
	var current [2][2]int64
	for i, path := range paths {
		major, minor, err := localoci.ResolveDeviceNode(path, "c", true)
		if err != nil {
			t.Fatal(err)
		}
		current[i] = [2]int64{major, minor}
	}
	spec := localoci.DefaultSpec("/rootfs", nil)
	spec.Linux.Devices = nil
	spec.Linux.Resources.Devices = nil
	for i, path := range []string{"/dev/fastrpc-cdsp", "/dev/fastrpc-cdsp1"} {
		old := current[1-i]
		spec.Mounts = append(spec.Mounts, localoci.Mount{Source: paths[i], Destination: path, Type: "bind", Options: []string{"rbind", "rw", "nosuid", "noexec"}})
		localoci.RecordPinnedDeviceMapping(spec, localoci.PinnedDevice{Path: path, HostPath: paths[i], Type: "c", Major: old[0], Minor: old[1], NoFollow: true})
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: new(old[0]), Minor: new(old[1]), Access: "rw"})
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	store := &deviceSpecStore{record: containers.Container{ID: "npu-app", Spec: &anypb.Any{TypeUrl: "test-spec", Value: raw}, SnapshotKey: "keep-model-volume"}}
	remote, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := remote.LoadContainer(context.Background(), "npu-app")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{client: remote, logger: zap.NewNop()}
	labels := map[string]string{"sh.wendy/entitlement.npu": ""}
	if err := client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "npu-app", labels); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.masks, []string{"spec"}) {
		t.Fatalf("NPU-only container did not persist refreshed bindings: masks=%v", store.masks)
	}
	var repaired localoci.Spec
	if err := json.Unmarshal(store.record.Spec.GetValue(), &repaired); err != nil {
		t.Fatal(err)
	}
	if len(repaired.Linux.Devices) != 0 || !reflect.DeepEqual(repaired.Mounts, spec.Mounts) || store.record.SnapshotKey != "keep-model-volume" {
		t.Fatal("refresh changed bind mounts, device entries, or snapshot")
	}
	if len(repaired.Linux.Resources.Devices) != 2 {
		t.Fatalf("unexpected device policy: %+v", repaired.Linux.Resources.Devices)
	}
	for _, rule := range repaired.Linux.Resources.Devices {
		if !rule.Allow || rule.Type != "c" || rule.Major == nil || rule.Minor == nil || rule.Access != "rw" {
			t.Fatalf("device access broadened: %+v", rule)
		}
		pair := [2]int64{*rule.Major, *rule.Minor}
		if pair != current[0] && pair != current[1] {
			t.Fatalf("stale device access rule: %+v", rule)
		}
	}
	var pins []localoci.PinnedDevice
	if err := json.Unmarshal([]byte(repaired.Annotations["sh.wendy/pinned-devices"]), &pins); err != nil {
		t.Fatal(err)
	}
	for _, pin := range pins {
		for i, path := range paths {
			if pin.HostPath == path && [2]int64{pin.Major, pin.Minor} != current[i] {
				t.Fatalf("stale FastRPC pin retained: %+v", pin)
			}
		}
	}
	store.masks = nil
	if err := client.refreshAcceleratorDeviceNumbersForStart(context.Background(), ctr, "npu-app", labels); err != nil {
		t.Fatal(err)
	}
	if store.masks != nil {
		t.Fatal("current NPU bindings were rewritten")
	}
}
