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
	}{
		{"repair", true, false}, {"required repair write fails", true, true}, {"optional annotation write fails", false, true},
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
			err = client.refreshGPUDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement.gpu": ""})
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
				if err := client.refreshGPUDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement.gpu": ""}); err != nil {
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
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid=", invalid), func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "device")
			if invalid {
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
			err = client.refreshGPUDeviceNumbersForStart(context.Background(), ctr, "app", map[string]string{"sh.wendy/entitlement.gpu": ""})
			if err == nil {
				t.Fatal("unresolved device accepted")
			}
			if !invalid && !errors.Is(err, ErrDeviceUnavailable) {
				t.Fatalf("missing device classification: %v", err)
			}
			if len(store.masks) != 0 || !bytes.Equal(store.record.Spec.GetValue(), raw) {
				t.Fatal("failed preflight persisted partial repairs")
			}
		})
	}
}
