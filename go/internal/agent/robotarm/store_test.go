package robotarm

import (
	"os"
	"path/filepath"
	"strings"
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

// The file holds proto JSON with proto field names, so records written before
// the store used protojson still load, and a newer Agent's fields don't make an
// older one treat the file as corrupt.
func TestStoreFileFormatAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	earlier := `[
  {
    "id": "usb:1a86:55d3:s:41:1",
    "name": "Follower",
    "model": "so101",
    "configured": true,
    "identity_kind": "usb_serial",
    "usb_interface": "1",
    "vendor_id": "1a86",
    "serial_number": "A",
    "added_by_a_newer_agent": {"x": 1}
  }
]`
	if err := os.WriteFile(path, []byte(earlier), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	records, err := s.List()
	if err != nil || len(records) != 1 || records[0].Name != "Follower" || records[0].IdentityKind != "usb_serial" || records[0].SerialNumber != "A" {
		t.Fatalf("earlier record: %v %v", records, err)
	}
	if err = s.Put(&pb.RobotArm{Id: "usb:b", Model: "so101", Configured: true, UsbLocation: "1-2", DevicePath: "/dev/ttyACM0"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"identity_kind": "usb_serial"`, `"usb_location": "1-2"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("want %s in:\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "device_path") || strings.Contains(string(data), "added_by_a_newer_agent") {
		t.Fatalf("persisted live or unknown fields:\n%s", data)
	}
	for _, bad := range []string{`[null]`, `null`, `[{"id": 7}]`} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.List(); err == nil {
			t.Fatalf("invalid store %s accepted", bad)
		}
	}
}
