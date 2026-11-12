package hoststats

import (
	"errors"
	"strings"
	"testing"
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
