package commands

import (
	"bytes"
	jsonpkg "encoding/json"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"strings"
	"testing"
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
