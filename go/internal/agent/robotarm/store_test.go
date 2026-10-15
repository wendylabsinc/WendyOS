package robotarm

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestStorePersistenceOfflineForgetAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "bindings.json")
	s := NewStore(path)
	d := &pb.RobotArm{VendorId: "1a86", ProductId: "55d3", SerialNumber: "A", UsbInterface: "1", DevicePath: "/dev/test"}
	Identify(d)
	d.Configured = true
	d.Model = "so101"
	d.Name = "Follower"
	if err := s.Put(d); err != nil {
		t.Fatal(err)
	}
	records, err := NewStore(path).List()
	if err != nil || len(records) != 1 {
		t.Fatalf("restart: %v %v", records, err)
	}
	if records[0].Connected || records[0].DevicePath != "" || records[0].Verified {
		t.Fatal("persisted live claims")
	}
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", stat, err)
	}
	if err := NewStore(path).Forget(d.Id); err != nil {
		t.Fatal(err)
	}
	records, err = s.List()
	if err != nil || len(records) != 0 {
		t.Fatal("offline forget failed")
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(d); err == nil {
		t.Fatal("corrupt store was overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "broken" {
		t.Fatal("corrupt state lost")
	}
}
func TestStoreWriteFailureAndConcurrentUpdates(t *testing.T) {
	root := t.TempDir()
	block := filepath.Join(root, "file")
	if err := os.WriteFile(block, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(filepath.Join(block, "bindings.json")).Put(&pb.RobotArm{Id: "x", Model: "so101"}); err == nil {
		t.Fatal("write failure hidden")
	}
	s := NewStore(filepath.Join(root, "bindings.json"))
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := s.Put(&pb.RobotArm{Id: id, Model: "so101", Configured: true}); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	records, err := s.List()
	if err != nil || len(records) != 4 {
		t.Fatalf("concurrent update lost: %v %v", records, err)
	}
}
