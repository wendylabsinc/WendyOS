package containerd

import (
	"context"
	"reflect"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestROS2SidecarInheritsAnchorDiscovery(t *testing.T) {
	for _, test := range []struct {
		name string
		env  []string
		host bool
	}{
		{"host robot sensors", []string{"ROS_DOMAIN_ID=0", "ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET"}, true},
		{"legacy host", []string{"ROS_LOCALHOST_ONLY=0"}, true},
		{"isolated app", []string{"ROS_LOCALHOST_ONLY=1", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST"}, false},
		{"legacy default", nil, false},
		{"conflicting settings", []string{"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST"}, false},
		{"disabled discovery", []string{"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=OFF"}, false},
		{"unknown discovery", []string{"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=INVALID"}, false},
		{"last setting wins", []string{"ROS_LOCALHOST_ONLY=1", "ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, err := typeurl.MarshalAny(&specs.Spec{Process: &specs.Process{Env: test.env}})
			if err != nil {
				t.Fatal(err)
			}
			client, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(ros2TestContainerStore{
				records: []containers.Container{{ID: "anchor", Spec: spec}},
			})))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			c := &Client{client: client}
			discovery, err := c.ros2SidecarDiscoveryEnv(context.Background(), map[string]string{labelKeyROS2AnchorID: "anchor"}, false)
			if err != nil {
				t.Fatal(err)
			}
			// Cached inspectors already have a localhost-only variable. The
			// executed subscriber must have exactly one effective setting.
			original := []string{"PATH=/usr/bin", "ROS_LOCALHOST_ONLY=1", "ROS_LOCALHOST_ONLY=1", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST", "ROS_DOMAIN_ID=42"}
			before := append([]string(nil), original...)
			got := ros2WithDiscoveryEnv(original, discovery)
			want := append([]string{"PATH=/usr/bin", "ROS_DOMAIN_ID=42"}, ros2ExecDiscoveryEnv(test.host)...)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(original, before) {
				t.Fatalf("incorrect subscriber environment: %v; original: %v", got, original)
			}
		})
	}
}

func TestROS2SidecarDiscoveryRequiresAnchorOnlyForApp(t *testing.T) {
	c := newROS2ContainerTestClient(t, nil, nil)
	for _, labels := range []map[string]string{nil, {labelKeyROS2AnchorID: "missing"}} {
		if _, err := c.ros2SidecarDiscoveryEnv(context.Background(), labels, false); err == nil {
			t.Fatal("missing anchor silently selected a discovery network")
		}
	}
	discovery, err := c.ros2SidecarDiscoveryEnv(context.Background(), nil, true)
	if err != nil || !reflect.DeepEqual(discovery, ros2ExecDiscoveryEnv(true)) {
		t.Fatalf("standalone host inspector unexpectedly needs an app: %v, %v", discovery, err)
	}
}
