package commands

import (
	"bytes"
	jsonpkg "encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func TestLiteNetStatusWarnsBeforeCarrierRestart(t *testing.T) {
	previous := jsonOutput
	defer func() { jsonOutput = previous }()
	for _, json := range []bool{false, true} {
		jsonOutput = json
		var output, warnings bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&output)
		cmd.SetErr(&warnings)
		if err := printLiteNetStatus(cmd, &litepb.WendyComNetStatus{Restarting: true}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(warnings.String(), "settings applied; the device is restarting") {
			t.Fatalf("missing restart warning: %q", warnings.String())
		}
		if json {
			var decoded map[string]any
			if err := jsonpkg.Unmarshal(output.Bytes(), &decoded); err != nil || decoded["restarting"] != true {
				t.Fatalf("missing machine-readable status: %q (%v)", output.String(), err)
			}
		}
	}
}

func TestLiteNetPartialUpdatePreservesOmittedSettings(t *testing.T) {
	root := newDeviceLocalMeshCmd()
	cmd, _, err := root.Find([]string{"configure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--participate=false", "--ble=true", "--max-links=2", "--ble-links=2", "--nan-links=0"}); err != nil {
		t.Fatal(err)
	}
	config := &litepb.WendyComNetConfig{Enabled: true, InfrastructureWifi: true, Nan: true, MaxLinks: 1, BleLinks: 1, WifiLinks: 3, NanLinks: 4}
	if err := updateLiteNetConfig(cmd, config); err != nil {
		t.Fatal(err)
	}
	if config.Enabled || !config.Ble || !config.InfrastructureWifi || !config.Nan || config.MaxLinks != 2 || config.BleLinks != 2 || config.WifiLinks != 3 || config.NanLinks != 0 {
		t.Fatalf("partial update lost explicit false/zero or omitted settings: %v", config)
	}
}

func TestLiteNetStatusRejectsMissingResponse(t *testing.T) {
	if err := printLiteNetStatus(&cobra.Command{}, nil); err == nil {
		t.Fatal("missing status was reported as a disabled net")
	}
}

func TestLocalNetConfigureRejectsEmptyUpdateBeforeResolvingDevice(t *testing.T) {
	root := newDeviceLocalMeshCmd()
	cmd, _, err := root.Find([]string{"configure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "specify a Wendy Net setting") {
		t.Fatalf("empty update = %v", err)
	}
}
