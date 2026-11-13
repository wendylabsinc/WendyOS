package containerd

import (
	"context"
	"testing"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestGroupRestartUsesPersistedServiceOwner(t *testing.T) {
	for _, tc := range []struct {
		name, app, service, isolation string
		members                       int
		present, want                 bool
	}{
		{"robot_alpha", "robot", "alpha", "shared-network", 2, true, true},
		{"my_robot_alpha", "my_robot", "alpha", "shared-ipc", 2, true, true},
		{"my_robot_alpha", "my_robot_alpha", "", "shared-network", 2, true, false},
		{"robot_alpha", "other", "alpha", "shared-network", 2, true, false},
		{"robot_alpha", "robot", "beta", "shared-network", 2, true, false},
		{"robot_alpha", "robot", "alpha", "", 2, true, false},
		{"robot_alpha", "robot", "alpha", "shared-network", 1, true, false},
		{"robot_alpha", "robot", "alpha", "shared-network", 2, false, false},
	} {
		t.Run(tc.name+"/"+tc.app+"/"+tc.isolation, func(t *testing.T) {
			store := ros2TestContainerStore{}
			if tc.present {
				store.records = []containers.Container{{ID: tc.name, Labels: map[string]string{labelKeyAppID: tc.app, labelKeyServiceName: tc.service, labelKeyIsolation: "shared-network"}}}
			}
			remote, err := containerdclient.New("", containerdclient.WithServices(containerdclient.WithContainerStore(store)))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			members := map[string]*appconfig.ServiceConfig{"alpha": {}}
			if tc.members > 1 {
				members["beta"] = &appconfig.ServiceConfig{}
			}
			c := &Client{client: remote, namespace: "owner-regression", appIsolation: map[string]string{tc.app: tc.isolation}, appServices: map[string]map[string]*appconfig.ServiceConfig{tc.app: members}}
			group, got := c.GroupRestartAppID(context.Background(), tc.name)
			if got != tc.want || (got && group != tc.app) {
				t.Fatalf("group restart owner=%q grouped=%v, want app=%q grouped=%v", group, got, tc.app, tc.want)
			}
		})
	}
}
