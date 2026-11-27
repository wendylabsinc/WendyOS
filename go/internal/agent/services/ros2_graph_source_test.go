package services

import (
	"context"
	"errors"
	"testing"
)

type graphRuntime struct {
	countingRuntime
	targets []ROS2Target
	err     error
}

func (r *graphRuntime) FindROS2Containers(context.Context) ([]ROS2Target, error) {
	return r.targets, r.err
}

func TestROS2GraphSourceListsRunningAppsWithWhatTheyMayRead(t *testing.T) {
	rt := &graphRuntime{targets: []ROS2Target{
		{ContainerID: "c-nav", AppID: "nav", DomainID: 7, Running: true, TaskPID: 4242, SensorRead: true},
		{ContainerID: "c-viz", AppID: "viz", DomainID: 7, Running: true, TaskPID: 5151, HostNetwork: true},
		{ContainerID: "c-stopped", AppID: "stopped", DomainID: 7, SensorRead: true},
		{ContainerID: "c-anon", DomainID: 9, Running: true, TaskPID: 6161},
	}}
	graphs, err := ros2GraphSource(rt)(context.Background())
	if err != nil || len(graphs) != 3 {
		t.Fatalf("%d graphs, %v; want the three running apps", len(graphs), err)
	}
	nav, viz, anon := graphs[0], graphs[1], graphs[2]
	if nav.Key != "nav" || nav.InstanceKey != "c-nav" || nav.DomainID != 7 || nav.NetworkNamespacePID != 4242 || nav.HostNetwork {
		t.Fatalf("nav = %+v", nav)
	}
	// Whether an app holds sensor-read travels with it, so a publisher of
	// sensor data can leave out the apps that do not.
	if !nav.SensorRead || viz.SensorRead || anon.SensorRead {
		t.Fatalf("sensor-read: nav %v, viz %v, anon %v; want only nav", nav.SensorRead, viz.SensorRead, anon.SensorRead)
	}
	if !viz.HostNetwork || anon.Key != "c-anon" {
		t.Fatalf("viz = %+v, anon = %+v", viz, anon)
	}

	// A graph is verified against the app that owns its network right now.
	if !nav.Verify(context.Background()) {
		t.Fatal("a running app did not verify")
	}
	rt.targets[0].TaskPID = 4343 // restarted: same container, new process
	if nav.Verify(context.Background()) {
		t.Fatal("an app whose process changed still verified")
	}
	rt.err = errors.New("containerd is restarting")
	if nav.Verify(context.Background()) {
		t.Fatal("an app verified although the apps could not be listed")
	}
	if _, err := ros2GraphSource(rt)(context.Background()); err == nil {
		t.Fatal("a failed listing was reported as no apps")
	}
}
