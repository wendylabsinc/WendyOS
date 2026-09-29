package vm

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestCreateFromWithRobotProfileRollsBackProfileWriteFailure(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	profile, err := NewRobotProfile(RobotKindG1, "sha256:"+strings.Repeat("0", 64), "bundle")
	if err != nil {
		t.Fatal(err)
	}
	image := lifecycleTestReader(func([]byte) (int, error) {
		// Fail the profile's atomic rename after the disk has been created.
		if err := os.Mkdir(store.RobotProfilePath("dev"), 0700); err != nil {
			t.Fatal(err)
		}
		return 0, io.EOF
	})
	if err := store.CreateFromWithRobotProfile("dev", image, 0, 1024, Meta{}, &profile); err == nil {
		t.Fatal("profile write failure was ignored")
	}
	if _, err := os.Stat(store.Dir("dev")); !os.IsNotExist(err) {
		t.Fatalf("partial robot VM survived failure: %v", err)
	}
	if err := store.CreateFromWithRobotProfile("dev", strings.NewReader("disk"), 4, 1024, Meta{}, &profile); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	got, exists, err := store.ReadRobotProfile("dev")
	if err != nil || !exists || got.Kind != RobotKindG1 {
		t.Fatalf("profile: %+v %v %v", got, exists, err)
	}
}
