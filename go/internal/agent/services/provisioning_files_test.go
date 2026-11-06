package services

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

var provisioningFiles = []struct {
	name  string
	data  []byte
	write func(string, []byte) error
}{
	{"device-key.pem", []byte("key-v1\n"), WriteDeviceKey},
	{"provisioning.json", []byte(`{"enrolled":true,"certPem":"cert-v1","chainPem":"ca-v1"}`), WriteProvisioningState},
}

func provisioningStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func provisioningOwners(info os.FileInfo) []uint64 {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		stat = stat.Elem()
	}
	var owners []uint64
	if stat.Kind() == reflect.Struct {
		for _, name := range []string{"Uid", "Gid"} {
			field := stat.FieldByName(name)
			if field.IsValid() {
				owners = append(owners, field.Uint())
			}
		}
	}
	return owners
}

func assertProvisioningUnchanged(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after := provisioningStat(t, path)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !reflect.DeepEqual(provisioningOwners(before), provisioningOwners(after)) {
		t.Fatalf("%s changed: inode/mode/mtime/owner", path)
	}
}

func assertNoLegacyProvisioningFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"device.pem", "ca.pem", ".provisioned"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unused file %s was emitted: %v", name, err)
		}
	}
}

func TestProvisioningFilesPreserveAndRepair(t *testing.T) {
	for _, entry := range provisioningFiles {
		for _, kind := range []string{"unchanged", "changed-bytes", "wrong-mode", "missing", "symlink"} {
			t.Run(entry.name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				if err := entry.write(dir, entry.data); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, entry.name)
				at := time.Unix(1700000000, 0)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
				before := provisioningStat(t, path)
				desired := append([]byte(nil), entry.data...)
				var target string
				switch kind {
				case "changed-bytes":
					// Same length exercises the byte comparison, not just size.
					if entry.name == "provisioning.json" {
						desired[strings.Index(string(desired), "cert-v1")] = 'C'
					} else {
						desired[len(desired)-1] ^= 1
					}
				case "wrong-mode":
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatal(err)
					}
				case "missing", "symlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if kind == "symlink" {
						target = filepath.Join(t.TempDir(), "target")
						if err := os.WriteFile(target, entry.data, 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
						before = provisioningStat(t, path)
					}
				}
				if err := entry.write(dir, desired); err != nil {
					t.Fatal(err)
				}
				after := provisioningStat(t, path)
				data, err := os.ReadFile(path)
				if err != nil || after.Mode() != 0o600 || string(data) != string(desired) {
					t.Fatalf("bad state file: %v %q %v", after.Mode(), data, err)
				}
				if kind == "unchanged" {
					assertProvisioningUnchanged(t, path, before)
				} else if kind != "missing" && os.SameFile(before, after) {
					t.Fatal("repair did not atomically replace inode")
				}
				if target != "" {
					data, err := os.ReadFile(target)
					if err != nil || string(data) != string(entry.data) {
						t.Fatal("symlink target changed")
					}
				}
				assertNoLegacyProvisioningFiles(t, dir)
			})
		}
	}
}

func TestProvisioningFilesFailureRemovesTemps(t *testing.T) {
	for _, entry := range provisioningFiles {
		t.Run(entry.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, entry.name)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			before := provisioningStat(t, path)
			if err := entry.write(dir, entry.data); err == nil || !strings.Contains(err.Error(), entry.name) {
				t.Fatalf("missing contextual rename error: %v", err)
			}
			assertProvisioningUnchanged(t, path, before)
			temps, err := filepath.Glob(filepath.Join(dir, ".pem-tmp-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files remain: %v %v", temps, err)
			}
			assertNoLegacyProvisioningFiles(t, dir)
		})
	}
	if err := WriteDeviceKey(t.TempDir(), nil); err == nil {
		t.Fatal("accepted an empty key")
	}
}

func TestProvisioningSaveAndLoadPreserveFiles(t *testing.T) {
	dir := t.TempDir()
	svc := NewProvisioningService(zap.NewNop(), dir)
	state := provisioningState{Enrolled: true, OrgID: 64, AssetID: 358, KeyPEM: "must-not-be-saved", CertPEM: "cert-v1", ChainPEM: "ca-v1"}
	complete, err := svc.persistProvisioning(&state, []byte("key-v1"))
	if err != nil {
		t.Fatal(err)
	}
	complete()
	before := map[string]os.FileInfo{}
	for _, name := range []string{"device-key.pem", "provisioning.json"} {
		path := filepath.Join(dir, name)
		at := time.Unix(1700000000, 0)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		before[name] = provisioningStat(t, path)
	}
	if err := svc.saveState(&state); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		loaded := NewProvisioningService(zap.NewNop(), dir)
		if !loaded.enrolled || string(loaded.keyPEM) != "key-v1" || loaded.certPEM != state.CertPEM || loaded.chainPEM != state.ChainPEM {
			t.Fatal("startup failed to load enrollment")
		}
		for name, info := range before {
			assertProvisioningUnchanged(t, filepath.Join(dir, name), info)
		}
	}
	data, err := os.ReadFile(svc.statePath())
	if err != nil || strings.Contains(string(data), "keyPem") {
		t.Fatalf("private key in JSON: %q %v", data, err)
	}
	assertNoLegacyProvisioningFiles(t, dir)
}

func TestProvisioningLegacyKeyMigration(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "provisioning.json")
			data, err := json.Marshal(provisioningState{Enrolled: true, KeyPEM: "legacy-key", CertPEM: "cert", ChainPEM: "chain"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if blocked {
				if err := os.Mkdir(filepath.Join(dir, "device-key.pem"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			svc := NewProvisioningService(zap.NewNop(), dir)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if blocked {
				if string(got) != string(data) {
					t.Fatal("legacy key was discarded before persistence succeeded")
				}
			} else {
				key, err := os.ReadFile(filepath.Join(dir, "device-key.pem"))
				if err != nil || string(key) != "legacy-key" || string(svc.keyPEM) != "legacy-key" {
					t.Fatalf("migration failed: %q %v", key, err)
				}
				if strings.Contains(string(got), "keyPem") {
					t.Fatal("legacy key retained in JSON")
				}
			}
			assertNoLegacyProvisioningFiles(t, dir)
		})
	}
}

func TestProvisioningKeyFailureDoesNotCommit(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "device-key.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	svc := NewProvisioningService(zap.NewNop(), dir)
	if _, err := svc.persistProvisioning(&provisioningState{Enrolled: true, CertPEM: "cert"}, []byte("key")); err == nil {
		t.Fatal("key write failure accepted")
	}
	if svc.enrolled {
		t.Fatal("failed enrollment committed in memory")
	}
	if _, err := os.Stat(svc.statePath()); !os.IsNotExist(err) {
		t.Fatal("failed enrollment committed on disk")
	}
}

func TestProvisioningStartupPreservesOriginalJSONAndRepairsPermissions(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"enrolled":true, "certPem":"cert", "futureField":{"keep":true}}`)
	if err := os.WriteFile(filepath.Join(dir, "device-key.pem"), []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provisioning.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	NewProvisioningService(zap.NewNop(), dir)
	for _, name := range []string{"device-key.pem", "provisioning.json"} {
		info := provisioningStat(t, filepath.Join(dir, name))
		if info.Mode() != 0o600 {
			t.Fatalf("%s permissions not repaired: %v", name, info.Mode())
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "provisioning.json"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("startup changed original JSON: %q %v", got, err)
	}
	assertNoLegacyProvisioningFiles(t, dir)
}
