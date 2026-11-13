package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestWiFiRegulatoryOutputShowsActualDomainAfterHint(t *testing.T) {
	old := jsonOutput
	defer func() { jsonOutput = old }()
	jsonOutput = false
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	r := &pb.WiFiRegulatoryStatus{GlobalCountry: "AU", RequestedCountry: "GB", Detail: "Hint only", Phys: []*pb.WiFiRegulatoryPhy{{Name: "phy0", Country: "US", SelfManaged: true, RegulatorySource: "phy", Channels: []*pb.WiFiRegulatoryChannel{{FrequencyMhz: 5180, Constraints: "(no IR)"}}}}}
	if err := printWiFiRegulatory(cmd, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Requested country hint: GB", "Global country hint: AU", "country=US source=phy self-managed=true", "5180 MHz (no IR)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestWiFiRegulatoryOutputRetainsChannelDetails(t *testing.T) {
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
	constraints := "Maximum TX power: 22.0 dBm\nNo IR\nChannel widths: 20MHz HT40+\nDFS state: unavailable (for 100 sec)\nDFS CAC time: 60000 ms"
	r := &pb.WiFiRegulatoryStatus{
		GlobalCountry: "AU", RequestedCountry: "GB", RawRegulatory: "global\ncountry AU: DFS-ETSI\n",
		Phys: []*pb.WiFiRegulatoryPhy{{Name: "camera radio", Country: "US", SelfManaged: true, RegulatorySource: "phy",
			RawPhy: "Wiphy camera radio\n\twiphy index: 1\n", RawChannels: "Band 1:\n\t* 5260 MHz [52]\n\t  " + strings.ReplaceAll(constraints, "\n", "\n\t  ") + "\n",
			Channels: []*pb.WiFiRegulatoryChannel{{FrequencyMhz: 5260, Constraints: constraints}},
		}},
	}
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	jsonOutput = false
	if err := printWiFiRegulatory(cmd, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  5260 MHz "+strings.ReplaceAll(constraints, "\n", "\n    ")+"\n") {
		t.Fatalf("channel details not grouped in text: %s", out)
	}
	out.Reset()
	jsonOutput = true
	if err := printWiFiRegulatory(cmd, r); err != nil {
		t.Fatal(err)
	}
	var got struct {
		GlobalCountry    string `json:"globalCountry"`
		RequestedCountry string `json:"requestedCountry"`
		RawRegulatory    string `json:"rawRegulatory"`
		Phys             []struct {
			Name        string `json:"name"`
			Country     string `json:"country"`
			SelfManaged bool   `json:"selfManaged"`
			RawPhy      string `json:"rawPhy"`
			RawChannels string `json:"rawChannels"`
			Channels    []struct {
				Frequency   float64 `json:"frequencyMhz"`
				Constraints string  `json:"constraints"`
			} `json:"channels"`
		} `json:"phys"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.GlobalCountry != "AU" || got.RequestedCountry != "GB" || got.RawRegulatory != r.RawRegulatory || len(got.Phys) != 1 {
		t.Fatalf("lost snapshot: %s", out)
	}
	phy := got.Phys[0]
	if phy.Name != "camera radio" || phy.Country != "US" || !phy.SelfManaged || phy.RawPhy != r.Phys[0].RawPhy || phy.RawChannels != r.Phys[0].RawChannels {
		t.Fatalf("lost radio evidence: %s", out)
	}
	if len(phy.Channels) != 1 || phy.Channels[0].Frequency != 5260 || phy.Channels[0].Constraints != constraints {
		t.Fatalf("lost channel details: %s", out)
	}
}

func TestWiFiRegulatoryCountryCommandValidatesBeforeConnecting(t *testing.T) {
	cmd, args, err := newWifiCmd().Find([]string{"regulatory", "set-country", "00"})
	if err != nil || cmd.Name() != "set-country" {
		t.Fatalf("command not registered: %v %v", cmd, err)
	}
	if err := cmd.Args(cmd, args); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, args); err == nil || !strings.Contains(err.Error(), "ISO 3166-1") {
		t.Fatalf("invalid country reached connection setup: %v", err)
	}
}

func TestWiFiRegulatoryTextUsesStdoutByDefault(t *testing.T) {
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
	jsonOutput = false
	var printErr error
	stdout, stderr := captureBoth(t, func() {
		printErr = printWiFiRegulatory(&cobra.Command{}, &pb.WiFiRegulatoryStatus{GlobalCountry: "AU", Detail: "Hint only"})
	})
	if printErr != nil {
		t.Fatal(printErr)
	}
	if stdout != "Global country hint: AU\nHint only\n" || stderr != "" {
		t.Fatalf("payload streams: stdout=%q stderr=%q", stdout, stderr)
	}
}
