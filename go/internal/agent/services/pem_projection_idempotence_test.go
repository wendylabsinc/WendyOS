package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

var projectionFiles = []struct {
	name, data string
	mode       os.FileMode
}{
	{"device-key.pem", "key-v1\n", 0o600},
	{"device.pem", "cert-v1\n", 0o644},
	{"ca.pem", "ca-v1\n", 0o644},
}

func projectionStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func projectionOwners(info os.FileInfo) []uint64 {
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

func assertProjectionUnchanged(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after := projectionStat(t, path)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) || !reflect.DeepEqual(projectionOwners(before), projectionOwners(after)) {
		t.Fatalf("%s changed: inode/mode/mtime/owner", path)
	}
}

func projectionFixture(t *testing.T) (string, map[string]os.FileInfo) {
	t.Helper()
	dir := t.TempDir()
	if err := WritePEMFiles(dir, "key-v1\n", "cert-v1\n", "ca-v1\n"); err != nil {
		t.Fatal(err)
	}
	before := map[string]os.FileInfo{}
	for _, name := range []string{"device-key.pem", "device.pem", "ca.pem", ".provisioned"} {
		path := filepath.Join(dir, name)
		at := time.Unix(1700000000, 0)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		before[name] = projectionStat(t, path)
	}
	return dir, before
}

func TestWritePEMFilesPreservesIdenticalProjection(t *testing.T) {
	dir, before := projectionFixture(t)
	marker := filepath.Join(dir, ".provisioned")
	if err := os.WriteFile(marker, []byte("original-enrollment-time\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before[".provisioned"] = projectionStat(t, marker)
	for range 2 {
		if err := WritePEMFiles(dir, "key-v1\n", "cert-v1\n", "ca-v1\n"); err != nil {
			t.Fatal(err)
		}
	}
	for name, info := range before {
		assertProjectionUnchanged(t, filepath.Join(dir, name), info)
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "original-enrollment-time\n" {
		t.Fatalf("marker changed: %q %v", contents, err)
	}
}

func TestWritePEMFilesRepairsProjection(t *testing.T) {
	for index, entry := range projectionFiles {
		for _, kind := range []string{"changed-bytes", "wrong-mode", "missing", "symlink"} {
			t.Run(entry.name+"/"+kind, func(t *testing.T) {
				dir, before := projectionFixture(t)
				path := filepath.Join(dir, entry.name)
				var target string
				var targetBefore os.FileInfo
				desired := []string{"key-v1\n", "cert-v1\n", "ca-v1\n"}
				switch kind {
				case "changed-bytes":
					desired[index] = "replacement-v2\n"
				case "wrong-mode":
					if err := os.Chmod(path, entry.mode^0o044); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					target = filepath.Join(t.TempDir(), "target")
					if err := os.WriteFile(target, []byte(entry.data), entry.mode); err != nil {
						t.Fatal(err)
					}
					targetBefore = projectionStat(t, target)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				original := before[entry.name]
				if err := WritePEMFiles(dir, desired[0], desired[1], desired[2]); err != nil {
					t.Fatal(err)
				}
				after := projectionStat(t, path)
				data, err := os.ReadFile(path)
				if err != nil || !after.Mode().IsRegular() || after.Mode() != entry.mode || string(data) != desired[index] {
					t.Fatalf("bad projection: %v %q %v", after.Mode(), data, err)
				}
				if kind != "missing" && os.SameFile(original, after) {
					t.Fatal("repair did not atomically replace inode")
				}
				for name, info := range before {
					if name != entry.name {
						assertProjectionUnchanged(t, filepath.Join(dir, name), info)
					}
				}
				if target != "" {
					assertProjectionUnchanged(t, target, targetBefore)
					data, err := os.ReadFile(target)
					if err != nil || string(data) != entry.data {
						t.Fatal("symlink target changed")
					}
				}
				temps, err := filepath.Glob(filepath.Join(dir, ".pem-tmp-*"))
				if err != nil || len(temps) != 0 {
					t.Fatalf("temporary files remain: %v %v", temps, err)
				}
			})
		}
	}
}

func TestWritePEMFilesEmptyFieldsKeepExistingFiles(t *testing.T) {
	dir, before := projectionFixture(t)
	if err := WritePEMFiles(dir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for name, info := range before {
		assertProjectionUnchanged(t, filepath.Join(dir, name), info)
	}
}

func TestWritePEMFilesMarkerCreationAndBestEffort(t *testing.T) {
	t.Run("missing-marker", func(t *testing.T) {
		dir := t.TempDir()
		if err := WritePEMFiles(dir, "", "", ""); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".provisioned"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("existing-non-file-marker", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, ".provisioned")
		if err := os.Mkdir(marker, 0o700); err != nil {
			t.Fatal(err)
		}
		before := projectionStat(t, marker)
		if err := WritePEMFiles(dir, "", "", ""); err != nil {
			t.Fatal(err)
		}
		assertProjectionUnchanged(t, marker, before)
	})
}

func TestWritePEMFilesFailureRemovesTemps(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "device-key.pem")
	if err := os.Mkdir(key, 0o700); err != nil {
		t.Fatal(err)
	}
	before := projectionStat(t, key)
	if err := WritePEMFiles(dir, "new-key", "new-cert", "new-ca"); err == nil || !strings.Contains(err.Error(), "writing device-key.pem") {
		t.Fatalf("missing contextual rename error: %v", err)
	}
	assertProjectionUnchanged(t, key, before)
	temps, err := filepath.Glob(filepath.Join(dir, ".pem-tmp-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files remain: %v %v", temps, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".provisioned")); !os.IsNotExist(err) {
		t.Fatal("marker created after failed projection")
	}
	file := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WritePEMFiles(file, "key", "cert", "ca"); err == nil || !strings.Contains(err.Error(), "creating config directory") {
		t.Fatalf("mkdir failure not propagated: %v", err)
	}
}

func TestProvisioningLoadStatePreservesPEMProjection(t *testing.T) {
	dir, before := projectionFixture(t)
	state := provisioningState{Enrolled: true, OrgID: 64, AssetID: 358, CertPEM: "cert-v1\n", ChainPEM: "ca-v1\n"}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provisioning.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewProvisioningService(zap.NewNop(), dir)
	if !svc.enrolled || string(svc.keyPEM) != "key-v1\n" {
		t.Fatal("startup failed to load enrollment")
	}
	for name, info := range before {
		assertProjectionUnchanged(t, filepath.Join(dir, name), info)
	}
}
