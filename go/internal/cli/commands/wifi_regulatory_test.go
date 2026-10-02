package commands

import (
	"bytes"
	"github.com/spf13/cobra"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"strings"
	"testing"
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
