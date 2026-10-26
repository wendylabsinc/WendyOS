package vm

import (
	"os"
	"strings"
	"testing"
)

func TestSO101CreationPersistsStoppedProfile(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	p, err := NewRobotProfile(RobotKindSO101, "sha256:"+strings.Repeat("c", 64), "so101-follower-a758567-v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFromWithRobotProfile("arm", strings.NewReader("test image"), 10, 1024, Meta{}, &p); err != nil {
		t.Fatal(err)
	}
	got, exists, err := s.ReadRobotProfile("arm")
	if err != nil || !exists || got != p {
		t.Fatalf("profile %+v %v %v", got, exists, err)
	}
	if _, err := os.Stat(s.StatePath("arm")); !os.IsNotExist(err) {
		t.Fatalf("create started VM: %v", err)
	}
}
