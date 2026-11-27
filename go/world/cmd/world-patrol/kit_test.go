package main

import (
	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolkit"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func preparedTestKit(t *testing.T) (*patrolkit.Catalog, patrolkit.Entry, patrolkit.Approval) {
	t.Helper()
	bundle, e := worldpackage.Load("../../examples/generic-office/world.json")
	if e != nil {
		t.Fatal(e)
	}
	var root map[string]any
	_ = json.Unmarshal(bundle.Root, &root)
	root["site_id"] = "studio-01"
	raw, _ := json.Marshal(root)
	bundle, e = worldpackage.Verify(raw, bundle.Files)
	if e != nil {
		t.Fatal(e)
	}
	frame := world.FrameRef{ID: "studio", Revision: bundle.Manifest.FrameRevision}
	id := "kit-0123456789abcdef01234567"
	run := "patrol-" + id
	digest := patrolkit.Digest([]byte("model"))
	binary := []byte("retained-test-executable")
	viewer := []byte("retained-test-viewer")
	runtime := patrolkit.Runtime{PhysicsImage: "sha256:" + digest, NavigationImage: "sha256:" + digest, ExecutorSHA256: patrolkit.Digest(binary), ViewerSHA256: patrolkit.Digest(viewer)}
	offline := &tasking.PatrolOffline{KitID: id, RunID: run, PackageSHA256: bundle.Manifest.SHA256, RuntimeSHA256: runtime.Digest(), ModelSHA256: digest, PreparationBootID: "preparation-only"}
	prepared := time.Now().UnixMilli()
	approval, spec, e := signFixture(run, frame, []patrol.Room{{ID: "studio"}}, 3600, scheduleDocument{Offline: offline})
	if e != nil {
		t.Fatal(e)
	}
	profile := patrolkit.Profile{Schema: patrolkit.Schema, ID: id, Name: "Office patrol", RunID: run, PackageSHA256: bundle.Manifest.SHA256, Runtime: runtime, ModelSHA256: digest, PreparationBootID: offline.PreparationBootID, Frame: frame, PreparedUnixMS: prepared, ExpiresUnixMS: spec.NotAfterUnixMS}
	c, e := patrolkit.Open(filepath.Join(t.TempDir(), "catalog"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	entry, e := c.Register(profile, bundle, approval, binary, viewer)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Register(profile, bundle, approval, binary, viewer); e == nil {
		t.Fatal("reused future kit identity was registered twice")
	}
	return c, entry, approval
}
func TestOfflineClaimRetainsAuthorityAndConsumesOnce(t *testing.T) {
	c, entry, original := preparedTestKit(t)
	out := filepath.Join(t.TempDir(), "activation")
	claim, e := c.Claim(entry.Profile.ID, out, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	v, actual, e := c.Activation(entry.Profile.ID, out, filepath.Join(out, "kit/runtime/world-patrol"), time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if actual != claim || !bytes.Equal(v.Approval.Instruction.Payload, original.Instruction.Payload) || v.Spec.NotAfterUnixMS != entry.Profile.ExpiresUnixMS {
		t.Fatal("activation changed original approval")
	}
	store, _, _, spec, e := admitApproval(out, claim.RunID, v.Approval, entry.RootPublicKey, v.Spec)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if spec.NotAfterUnixMS != entry.Profile.ExpiresUnixMS {
		t.Fatal("admission extended expiry")
	}
	if _, e = c.Claim(entry.Profile.ID, filepath.Join(t.TempDir(), "second"), time.Now()); e == nil {
		t.Fatal("repeat approval consumed twice")
	}
	if _, _, e = c.Activation(entry.Profile.ID, t.TempDir(), filepath.Join(out, "kit/runtime/world-patrol"), time.Now()); e == nil {
		t.Fatal("claim copied to another output")
	}
	items, e := c.Inspect(time.Now())
	if e != nil || len(items) != 1 || items[0].State != "consumed" {
		t.Fatal(items, e)
	}
}
func TestOfflineRejectsAlteredMissingOrUnpinnedClosure(t *testing.T) {
	for _, mode := range []string{"changed", "missing", "extra", "symlink", "root-pin", "missing-pin", "wrong-frame", "package-binding"} {
		t.Run(mode, func(t *testing.T) {
			c, entry, _ := preparedTestKit(t)
			dir := filepath.Join(c.Path, "kits", entry.Profile.ID)
			switch mode {
			case "changed":
				_ = os.WriteFile(filepath.Join(dir, "world/office.xml"), []byte("changed"), 0600)
			case "missing":
				_ = os.Remove(filepath.Join(dir, "runtime/patrol_viewer.py"))
			case "extra":
				_ = os.WriteFile(filepath.Join(dir, "world/undeclared.py"), []byte("extra"), 0600)
			case "symlink":
				_ = os.Remove(filepath.Join(dir, "world/office.xml"))
				_ = os.Symlink("room.xml", filepath.Join(dir, "world/office.xml"))
			case "missing-pin":
				_ = os.Remove(filepath.Join(c.Path, "pins", entry.Profile.ID+".json"))
			case "wrong-frame":
				entry.Profile.Frame.Revision = "other"
				raw, _ := json.Marshal(entry)
				_ = os.WriteFile(filepath.Join(c.Path, "pins", entry.Profile.ID+".json"), raw, 0600)
			case "root-pin":
				entry.RootPublicKey = bytes.Repeat([]byte{2}, 32)
				raw, _ := json.Marshal(entry)
				_ = os.WriteFile(filepath.Join(c.Path, "pins", entry.Profile.ID+".json"), raw, 0600)
			case "package-binding":
				entry.Profile.PackageSHA256 = patrolkit.Digest([]byte("other"))
				raw, _ := json.Marshal(entry)
				_ = os.WriteFile(filepath.Join(c.Path, "pins", entry.Profile.ID+".json"), raw, 0600)
			}
			if _, e := c.Claim(entry.Profile.ID, filepath.Join(t.TempDir(), "activation"), time.Now()); e == nil {
				t.Fatal("altered approval activated")
			}
			if _, e := os.Stat(filepath.Join(c.Path, "claims", entry.Profile.ID+".json")); !os.IsNotExist(e) {
				t.Fatal("invalid kit reached claim")
			}
		})
	}
}
func TestOfflineExpiryClockAndSnapshot(t *testing.T) {
	t.Run("expired-inspection-cannot-rollback", func(t *testing.T) {
		c, entry, _ := preparedTestKit(t)
		if _, e := c.Verify(entry.Profile.ID, time.UnixMilli(entry.Profile.ExpiresUnixMS+1)); e == nil {
			t.Fatal("expired inspection accepted")
		}
		if _, e := c.Claim(entry.Profile.ID, filepath.Join(t.TempDir(), "activation"), time.Now()); e == nil {
			t.Fatal("clock rollback resurrected expired approval")
		}
	})

	t.Run("expired", func(t *testing.T) {
		c, entry, _ := preparedTestKit(t)
		if _, e := c.Claim(entry.Profile.ID, filepath.Join(t.TempDir(), "activation"), time.UnixMilli(entry.Profile.ExpiresUnixMS)); e == nil {
			t.Fatal("expired approval claimed")
		}
	})
	t.Run("changed-snapshot", func(t *testing.T) {
		c, entry, _ := preparedTestKit(t)
		out := filepath.Join(t.TempDir(), "activation")
		_, e := c.Claim(entry.Profile.ID, out, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(out, "kit/runtime/patrol_viewer.py")
		_ = os.Chmod(path, 0600)
		_ = os.WriteFile(path, []byte("changed"), 0600)
		if _, _, e = c.Activation(entry.Profile.ID, out, filepath.Join(out, "kit/runtime/world-patrol"), time.Now()); e == nil {
			t.Fatal("changed snapshot executed")
		}
	})
	t.Run("clock-rollback", func(t *testing.T) {
		c, entry, _ := preparedTestKit(t)
		out := filepath.Join(t.TempDir(), "activation")
		claim, e := c.Claim(entry.Profile.ID, out, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if _, _, e = c.Activation(entry.Profile.ID, out, filepath.Join(out, "kit/runtime/world-patrol"), time.UnixMilli(claim.ClaimedUnixMS-1)); e == nil {
			t.Fatal("rollback accepted")
		}
	})
}

func TestOfflineWorldWatchKeepsCommissionedFutureIdentity(t *testing.T) {
	future := "patrol-kit-0123456789abcdef01234567"
	p := &patrolwatch.Policy{Contract: gasreplica.Config{Field: gasfield.Config{Simulation: world.SimulationContext{RunID: future}}}}
	id, run, e := watchKitIdentity(p, "preparation-run")
	if e != nil || id != "kit-0123456789abcdef01234567" || run != future || p.Contract.Field.Simulation.RunID != future {
		t.Fatal("future identity changed", e)
	}
	if _, _, e = watchKitIdentity(p, future); e == nil {
		t.Fatal("preparation observation run reused as fresh activation")
	}
	p.Contract.Field.Simulation.RunID = "patrol-online-current"
	if _, _, e = watchKitIdentity(p, "preparation-run"); e == nil {
		t.Fatal("ordinary run silently relabeled to a future kit")
	}
}
