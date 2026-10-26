package mesh

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVIPAllocationWaitsForDurableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires POSIX directory permissions without root's DAC override")
	}
	table, path := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	first := deviceForIndex(t, tenant, 1)
	second := deviceForIndex(t, tenant, 2)
	original, err := table.Allocate(first)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	// Write/search permits the atomic rename, but opening the directory to
	// sync it fails. DNS must not receive a VIP whose name is not yet durable.
	if _, err := table.Allocate(second); err == nil {
		t.Fatal("issued a new VIP without a durable directory entry")
	}
	if got, ok := table.Lookup(first); ok && got != original {
		t.Fatalf("failed allocation reassigned an existing owner: %s, %v", got, ok)
	}
	if _, ok := table.Lookup(second); ok {
		t.Fatal("failed allocation became visible in the running table")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Reopening makes the visible snapshot durable before returning its
	// unissued reservation. The old VIP remains pinned throughout recovery.
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Lookup(first); !ok || got != original {
		t.Fatalf("restart lost the original owner: %s, %v", got, ok)
	}
	reserved, err := reopened.Allocate(second)
	if err != nil || reserved == original {
		t.Fatalf("recovered allocation: %s, %v", reserved, err)
	}
	third, err := reopened.Allocate(deviceForIndex(t, tenant, 3))
	if err != nil || third == reserved || third == original {
		t.Fatalf("reused a reserved VIP after recovery: %s, %v", third, err)
	}
}
