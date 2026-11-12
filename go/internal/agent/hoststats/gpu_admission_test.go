package hoststats

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const admissionXML = `<nvidia_smi_log><gpu><uuid>GPU-a</uuid><display_active>Disabled</display_active><mig_mode><current_mig>N/A</current_mig></mig_mode><fb_memory_usage><free>N/A</free></fb_memory_usage><utilization><gpu_util>0 %</gpu_util></utilization><processes><process_info><pid>12</pid><type>G</type></process_info></processes></gpu></nvidia_smi_log>`

func TestGPUAdmissionOwnershipAndUnifiedMemory(t *testing.T) {
	for _, test := range []struct {
		name, group string
		err         error
		foreign     bool
	}{
		{"own", "/app", nil, false}, {"descendant", "/app/child", nil, false},
		{"similar prefix", "/app-other", nil, true}, {"foreign graphics idle", "/other", nil, true},
		{"vanished", "", errors.New("gone"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := parseGPUAdmission([]byte("0, GPU-a\n"), []byte(admissionXML), []string{"/app"}, true, func(uint32) (string, error) { return test.group, test.err })
			if err != nil {
				t.Fatal(err)
			}
			g := result[0]
			if g.Foreign != test.foreign || !g.Unified || !g.Supported || g.FreeBytes != nil || g.Utilization == nil || *g.Utilization != 0 {
				t.Fatalf("unexpected: %+v", g)
			}
		})
	}
}

func TestGPUAdmissionIncompleteInventoryRejected(t *testing.T) {
	for _, xml := range []string{
		strings.ReplaceAll(admissionXML, "<processes>", "<processes>N/A"),
		strings.ReplaceAll(admissionXML, "<pid>12</pid>", "<pid>N/A</pid>"),
		strings.ReplaceAll(admissionXML, "GPU-a", "GPU-b"),
		"<nvidia_smi_log></nvidia_smi_log>",
		"<broken",
	} {
		if _, err := parseGPUAdmission([]byte("0, GPU-a\n"), []byte(xml), []string{"/app"}, false, func(uint32) (string, error) { return "/app", nil }); err == nil {
			t.Fatalf("accepted incomplete %s", xml)
		}
	}
}

// The full XML lists every supported clock pair, so one desktop GPU can exceed
// the 64 KiB cap used for the CSV queries (an RTX 4090 on driver 535: ~73 KiB).
func TestGPUAdmissionInventoryLargerThanToolCap(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat unavailable")
	}
	clocks := strings.Repeat("<supported_graphics_clock>2520 MHz</supported_graphics_clock>", 2000)
	inventory := strings.Replace(admissionXML, "<uuid>", "<supported_clocks>"+clocks+"</supported_clocks><uuid>", 1)
	if len(inventory) <= maxGPUToolOutput {
		t.Fatalf("fixture is %d bytes, want more than %d", len(inventory), maxGPUToolOutput)
	}
	dir := t.TempDir()
	inventoryPath := filepath.Join(dir, "inventory.xml")
	if err := os.WriteFile(inventoryPath, []byte(inventory), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) printf '0, GPU-a\\n' ;;\n-q) " + cat + " '" + inventoryPath + "' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	devices, err := ReadGPUAdmission(context.Background(), []string{"/app"})
	if err != nil || len(devices) != 1 || devices[0].UUID != "GPU-a" {
		t.Fatalf("large inventory rejected: %+v, %v", devices, err)
	}
	if _, err := runLimited(exec.Command(cat, inventoryPath), int64(len(inventory)-1)); err == nil {
		t.Fatal("accepted a truncated inventory")
	}
}

func TestGPUAdmissionOverlongOutputStopsTool(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("yes unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := runLimited(exec.CommandContext(ctx, "yes"), 1024); err == nil {
		t.Fatal("accepted unbounded output")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("tool ran until the deadline (%s) instead of being stopped", elapsed)
	}
}

func TestGPUAdmissionUnsupportedAndUnknownUtilization(t *testing.T) {
	xml := strings.ReplaceAll(admissionXML, "<display_active>Disabled", "<display_active>Enabled")
	xml = strings.ReplaceAll(xml, "0 %", "N/A")
	xml = strings.ReplaceAll(xml, "<free>N/A", "<free>128 MiB")
	result, err := parseGPUAdmission([]byte("0, GPU-a\n"), []byte(xml), []string{"/app"}, false, func(uint32) (string, error) { return "/app", nil })
	if err != nil {
		t.Fatal(err)
	}
	g := result[0]
	if g.Supported || g.Unified || g.Utilization != nil || g.FreeBytes == nil || *g.FreeBytes != 128*1024*1024 {
		t.Fatalf("unexpected %+v", g)
	}
}
