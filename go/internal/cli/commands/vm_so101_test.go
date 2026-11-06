package commands

import (
	"context"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	so101bundle "github.com/wendylabsinc/wendy/go/simulator/so101"
)

func TestSO101ConfigureDoesNotBootAndPinsOwnRuntime(t *testing.T) {
	store := robotTestStore(t)
	if err := os.MkdirAll(store.Dir("arm"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMeta(vm.Meta{Name: "arm"}); err != nil {
		t.Fatal(err)
	}
	if err := attachSimulatorProfile("arm", vm.RobotKindSO101); err != nil {
		t.Fatal(err)
	}
	profile, exists, err := store.ReadRobotProfile("arm")
	if err != nil || !exists || profile.Kind != vm.RobotKindSO101 || profile.SourceDigest != so101bundle.SourceDigest() || profile.PolicyBundle != so101ModelBundle {
		t.Fatalf("profile %+v %v %v", profile, exists, err)
	}
	if _, err := os.Stat(store.StatePath("arm")); !os.IsNotExist(err) {
		t.Fatalf("configuration booted VM: %v", err)
	}
	state := robotTestStatus("arm", profile)
	if err := state.matches("arm", profile); err != nil {
		t.Fatal(err)
	}
	state.RobotKind = vm.RobotKindGo2
	if err := state.matches("arm", profile); err == nil {
		t.Fatal("accepted foreign robot")
	}
	info := readSimulatorRobots(context.Background(), []vm.Status{{Name: "arm"}})
	if info["arm"].Kind != "LeRobot SO-101 follower" || info["arm"].State != "stopped" {
		t.Fatalf("picker: %+v", info)
	}
}
