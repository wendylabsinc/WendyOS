package backend

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTwinBundleValidation(t *testing.T) {
	for _, scenario := range []string{"valid", "corrupt-layer", "wrong-architecture", "wrong-app", "missing-bundle", "unsupported-architecture"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			layer := []byte("fixture layer")
			app := TwinAppID
			arch := "arm64"
			if scenario == "wrong-app" {
				app = "another-app"
			}
			imageArch := arch
			if scenario == "wrong-architecture" {
				imageArch = "amd64"
			}
			image, _ := json.Marshal(map[string]any{"architecture": imageArch, "os": "linux", "rootfs": map[string]any{"diff_ids": []string{"sha256:fixture"}}})
			config, _ := json.Marshal(map[string]string{"appId": app, "version": "0.1.0-fixture", "platform": "linux"})
			metadata, _ := json.Marshal(twinBundle{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(layer)), DiffID: "sha256:fixture", Size: int64(len(layer)), ImageConfig: image, AppConfig: config})
			if scenario == "corrupt-layer" {
				layer[0] ^= 1
			}
			if err := os.WriteFile(filepath.Join(directory, "arm64.tar.gz"), layer, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-bundle" {
				if err := os.WriteFile(filepath.Join(directory, "arm64.json"), metadata, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unsupported-architecture" {
				arch = "../arm64"
			}
			_, _, version, err := loadTwinBundle(directory, arch)
			if scenario == "valid" {
				if err != nil || version != "0.1.0-fixture" {
					t.Fatalf("%s %v", version, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid bundle")
			}
		})
	}
}

// Explicit opt-in exercises the same installer used by the Apple app on a device.
func TestTwinDeviceInstall(t *testing.T) {
	device := os.Getenv("WENDY_TWIN_INSTALL_DEVICE")
	if device == "" {
		t.Skip("physical device installation is opt-in")
	}
	b := New()
	defer b.Shutdown()
	cli, err := exec.LookPath("wendy")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(cli); err != nil {
		t.Fatal(err)
	}
	t.Log("connecting")
	session, err := b.Connect(device, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("connected")
	for i := 0; i < 2; i++ {
		endpoint, err := b.OpenTwin(session, os.Getenv("WENDY_TWIN_INSTALL_BUNDLE"))
		if err != nil {
			t.Fatal(err)
		}
		t.Log("ready", endpoint)
		response, err := (&http.Client{Timeout: 10 * time.Second}).Get(endpoint + "v1/rooms")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("rooms: %s %s", response.Status, body)
		}
		t.Logf("installation/open %d: rooms API %s", i+1, body)
	}
}
