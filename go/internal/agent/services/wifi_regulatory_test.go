package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const regulatoryFixture = `global
country AU: DFS-ETSI
    (2400 - 2483 @ 40), (N/A, 20), (N/A)
phy#0 (self-managed)
country US: DFS-FCC
    (5725 - 5850 @ 80), (N/A, 30), (N/A)
`
const phyFixture = `Wiphy phy0
    wiphy index: 0
    Band 1:
        Frequencies:
            * 5180.0 MHz [36] (22.0 dBm) (no IR)
            * 5260.0 MHz [52] (22.0 dBm) (no IR, radar detection)
            * 5745.0 MHz [149] (22.0 dBm)
            * 5845.0 MHz [169] (disabled)
Wiphy phy1
    wiphy index: 1
    Band 1:
        Frequencies:
            * 2412 MHz [1] (20.0 dBm)
`

// These fixtures follow iw's print_channels_handler output (phy.c), which is
// distinct from the print_phy_handler inventory format (info.c) above.
const channelsFixture = `Band 1:
	* 5180 MHz [36]
	  Maximum TX power: 22.0 dBm
	  No IR
	  Channel widths: 20MHz HT40+ VHT80
	* 5260 MHz [52]
	  Maximum TX power: 22.0 dBm
	  No IR
	  Radar detection
	  Channel widths: 20MHz HT40+
	  DFS state: unavailable (for 100 sec)
	  DFS CAC time: 60000 ms
	* 5745 MHz [149]
	  Maximum TX power: 22.0 dBm
	  Channel widths: 20MHz HT40+ VHT80
	* 5845 MHz [169] (disabled)
`
const channelsFixture1 = `Band 1:
	* 2412 MHz [1]
	  Maximum TX power: 20.0 dBm
	  Channel widths: 20MHz HT40+
`

func regulatoryFixtureCommand(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	switch {
	case reflect.DeepEqual(args, []string{"reg", "get"}):
		return []byte(regulatoryFixture), nil
	case reflect.DeepEqual(args, []string{"phy"}):
		return []byte(phyFixture), nil
	case reflect.DeepEqual(args, []string{"phy", "phy0", "channels"}):
		return []byte(channelsFixture), nil
	case reflect.DeepEqual(args, []string{"phy", "phy1", "channels"}):
		return []byte(channelsFixture1), nil
	case reflect.DeepEqual(args, []string{"reg", "set", "GB"}):
		return nil, nil
	default:
		t.Fatalf("unexpected iw argv %v", args)
		return nil, nil
	}
}

func TestWiFiRegulatorySnapshotSeparatesGlobalAndSelfManagedPHY(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	var calls [][]string
	s.regulatoryCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded command")
		}
		calls = append(calls, args)
		return regulatoryFixtureCommand(t, args...)
	}
	r, err := s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"reg", "get"}, {"phy"}, {"phy", "phy0", "channels"}, {"phy", "phy1", "channels"}}) || r.GlobalCountry != "AU" || len(r.Phys) != 2 {
		t.Fatalf("snapshot: %v", r)
	}
	if !r.Phys[0].SelfManaged || r.Phys[0].Country != "US" || r.Phys[0].RegulatorySource != "phy" {
		t.Fatalf("self-managed lost: %v", r.Phys[0])
	}
	if r.Phys[1].SelfManaged || r.Phys[1].Country != "AU" || r.Phys[1].RegulatorySource != "global" {
		t.Fatalf("global fallback: %v", r.Phys[1])
	}
	if len(r.Phys[0].Channels) != 4 || r.Phys[0].Channels[0].Constraints != "Maximum TX power: 22.0 dBm\nNo IR\nChannel widths: 20MHz HT40+ VHT80" || r.Phys[0].Channels[3].Constraints != "(disabled)" {
		t.Fatalf("constraints lost: %v", r.Phys[0])
	}
	for _, want := range []string{"Radar detection", "Channel widths: 20MHz HT40+", "DFS state: unavailable (for 100 sec)", "DFS CAC time: 60000 ms"} {
		if !strings.Contains(r.Phys[0].Channels[1].Constraints, want) {
			t.Fatalf("missing %q: %v", want, r.Phys[0].Channels[1])
		}
	}
	if r.RawRegulatory != regulatoryFixture || r.Phys[0].RawPhy == "" || r.Phys[0].RawChannels != channelsFixture || r.Phys[1].RawChannels != channelsFixture1 {
		t.Fatal("raw kernel evidence lost")
	}
}
func TestWiFiCountryRequestIsExplicitHintAndReturnsActualPHY(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	calls := [][]string{}
	s.regulatoryCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return regulatoryFixtureCommand(t, args...)
	}
	r, err := s.RequestWiFiCountry(context.Background(), &pb.RequestWiFiCountryRequest{CountryCode: "gb"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"reg", "set", "GB"}, {"reg", "get"}, {"phy"}, {"phy", "phy0", "channels"}, {"phy", "phy1", "channels"}}) {
		t.Fatal(calls)
	}
	if r.RequestedCountry != "GB" || r.GlobalCountry != "AU" || r.Phys[0].Country != "US" {
		t.Fatalf("request incorrectly represented as applied: %v", r)
	}
}
func TestWiFiRegulatoryCommandFailure(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	s.regulatoryCommand = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("iw missing") }
	_, err := s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
}

func TestWiFiRegulatoryRenamedPHY(t *testing.T) {
	renamed := "Wiphy camera-radio\n    wiphy index: 0\n    Frequencies:\n        * 5745.0 MHz [149] (22.0 dBm)\n"
	r, err := parseWiFiRegulatory(regulatoryFixture, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Phys) != 1 || r.Phys[0].Name != "camera-radio" || r.Phys[0].Country != "US" || !r.Phys[0].SelfManaged {
		t.Fatal(r)
	}
}

func TestWiFiRegulatoryRPC(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	s.regulatoryCommand = func(_ context.Context, args ...string) ([]byte, error) {
		return regulatoryFixtureCommand(t, args...)
	}
	client, close := startWiFiServerWithService(t, s)
	defer close()
	q, err := client.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
	if err != nil || q.GlobalCountry != "AU" || q.Phys[0].Country != "US" {
		t.Fatalf("query wire response %v %v", q, err)
	}
	if q.Phys[0].RawChannels != channelsFixture || !strings.Contains(q.Phys[0].Channels[1].Constraints, "DFS state: unavailable") {
		t.Fatalf("channel evidence lost over RPC: %v", q)
	}
	r, err := client.RequestWiFiCountry(context.Background(), &pb.RequestWiFiCountryRequest{CountryCode: "GB"})
	if err != nil || r.RequestedCountry != "GB" || r.Phys[0].Country != "US" {
		t.Fatalf("hint wire response %v %v", r, err)
	}
}

func TestWiFiRegulatoryWhitespacePHYName(t *testing.T) {
	for _, name := range []string{"camera radio", " camera radio "} {
		t.Run(name, func(t *testing.T) {
			reg := "global\ncountry AU: DFS-ETSI\nphy#1 (self-managed)\ncountry US: DFS-FCC\n"
			phy := "Wiphy phy0\n\twiphy index: 0\n\t* 2412 MHz [1] (20.0 dBm)\nWiphy " + name + "\n\twiphy index: 1\n\t* 5180 MHz [36] (22.0 dBm)\n"
			s := &WiFiService{regulatoryCommand: func(_ context.Context, args ...string) ([]byte, error) {
				switch {
				case reflect.DeepEqual(args, []string{"reg", "get"}):
					return []byte(reg), nil
				case reflect.DeepEqual(args, []string{"phy"}):
					return []byte(phy), nil
				case reflect.DeepEqual(args, []string{"phy", "phy0", "channels"}):
					return []byte(channelsFixture1), nil
				case reflect.DeepEqual(args, []string{"phy", name, "channels"}):
					return []byte(channelsFixture), nil
				default:
					t.Fatalf("unexpected iw argv %v", args)
					return nil, nil
				}
			}}
			r, err := s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Phys) != 2 {
				t.Fatalf("merged radios: %v", r)
			}
			if r.Phys[0].Name != "phy0" || r.Phys[0].Country != "AU" || r.Phys[0].SelfManaged || len(r.Phys[0].Channels) != 1 {
				t.Fatalf("wrong global radio: %v", r.Phys[0])
			}
			if r.Phys[1].Name != name || r.Phys[1].Country != "US" || !r.Phys[1].SelfManaged || len(r.Phys[1].Channels) != 4 {
				t.Fatalf("wrong self-managed radio: %v", r.Phys[1])
			}
		})
	}
}

func TestWiFiRegulatoryDetailedChannelFailure(t *testing.T) {
	for _, malformed := range []string{"", "unexpected output", "Band 1:\n* invalid channel"} {
		t.Run(malformed, func(t *testing.T) {
			s := &WiFiService{regulatoryCommand: func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) == 3 && args[2] == "channels" {
					return []byte(malformed), nil
				}
				return regulatoryFixtureCommand(t, args...)
			}}
			_, err := s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("accepted missing constraints: %v", err)
			}
		})
	}
	for _, request := range []bool{false, true} {
		s := &WiFiService{regulatoryCommand: func(_ context.Context, args ...string) ([]byte, error) {
			if len(args) == 3 && args[2] == "channels" {
				return nil, errors.New("radio disappeared")
			}
			return regulatoryFixtureCommand(t, args...)
		}}
		var err error
		if request {
			_, err = s.RequestWiFiCountry(context.Background(), &pb.RequestWiFiCountryRequest{CountryCode: "GB"})
		} else {
			_, err = s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
		}
		if status.Code(err) != codes.Unavailable {
			t.Fatal(err)
		}
		if request && !strings.Contains(err.Error(), "country hint GB submitted") {
			t.Fatalf("lost submitted hint: %v", err)
		}
	}
}

func TestWiFiRegulatoryChannelContinuation(t *testing.T) {
	raw := "Band 1:\n\t* 5260 MHz [52]\n\t  DFS state: usable\n\t  CAC ongoing (elapsed time: 2000 ms)\n\t  Future restriction\nBand 2:\n\t* 5745 MHz [149] (disabled)\n"
	channels, err := parseWiFiRegulatoryChannels(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0].Constraints != "DFS state: usable\nCAC ongoing (elapsed time: 2000 ms)\nFuture restriction" || channels[1].Constraints != "(disabled)" {
		t.Fatalf("lost or misattributed continuations: %v", channels)
	}
}
