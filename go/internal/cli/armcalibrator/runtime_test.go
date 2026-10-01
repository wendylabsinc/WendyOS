package armcalibrator

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCalibrateRuntimeRunsEmbeddedWizard(t *testing.T) {
	python, err := FindPython(context.Background())
	if err != nil {
		t.Skip("python3 is required to exercise the bundled wizard")
	}
	output := filepath.Join(t.TempDir(), "spaces and 'quotes' $(not-a-shell)")
	argv := Command([]string{"arms", "6", "--demo", "--auto", "--json", "--output", output})
	command := exec.Command(python, argv[1:]...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("wizard failed: %v\n%s", err, stderr.String())
	}
	var result struct {
		Path   string `json:"calibration_path"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "demo_only" || filepath.Dir(filepath.Dir(result.Path)) != output {
		t.Fatalf("wrong result: %+v", result)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}
}

func TestCalibrateRuntimeRefusesCorruptBundle(t *testing.T) {
	python, err := FindPython(context.Background())
	if err != nil {
		t.Skip("python3 is required")
	}
	argv := Command([]string{"profiles"})
	argv[4] = "wrong-digest"
	data, err := exec.Command(python, argv[1:]...).CombinedOutput()
	if err == nil || !bytes.Contains(data, []byte("checksum mismatch")) {
		t.Fatalf("bad digest accepted: %v %s", err, data)
	}
}

func TestCalibrateRuntimeRejectsUnattendedHardware(t *testing.T) {
	python, err := FindPython(context.Background())
	if err != nil {
		t.Skip("python3 is required")
	}
	argv := Command([]string{"arms", "6", "--auto"})
	data, err := exec.Command(python, argv[1:]...).CombinedOutput()
	if err == nil || !bytes.Contains(data, []byte("only available with --demo")) {
		t.Fatalf("unattended hardware accepted: %v %s", err, data)
	}
}

func TestCalibrateRuntimeIncludesLicenseAndProvenance(t *testing.T) {
	bundle, err := zip.NewReader(bytes.NewReader(runtime), int64(len(runtime)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "NOTICE", "wendy_arm_calibrator/I2RT_LICENSE.txt", "wendy_arm_calibrator/profiles.json"} {
		file, err := bundle.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	data, err := os.ReadFile("UPSTREAM.json")
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(data, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.SHA256 != fmt.Sprintf("%x", sha256.Sum256(runtime)) {
		t.Fatal("bundle does not match recorded upstream hash")
	}
}
