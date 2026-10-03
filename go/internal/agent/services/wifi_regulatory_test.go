package services

import (
	"context"
	"errors"
	"reflect"
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
    Band 1:
        Frequencies:
            * 5180.0 MHz [36] (22.0 dBm) (no IR)
            * 5260.0 MHz [52] (22.0 dBm) (no IR, radar detection)
                DFS state: usable (for 100 sec)
            * 5745.0 MHz [149] (22.0 dBm)
            * 5845.0 MHz [169] (disabled)
Wiphy phy1
    Band 1:
        Frequencies:
            * 2412 MHz [1] (20.0 dBm)
`

func TestWiFiRegulatorySnapshotSeparatesGlobalAndSelfManagedPHY(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	var calls [][]string
	s.regulatoryCommand = func(ctx context.Context, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded command")
		}
		calls = append(calls, args)
		if reflect.DeepEqual(args, []string{"reg", "get"}) {
			return []byte(regulatoryFixture), nil
		}
		if reflect.DeepEqual(args, []string{"phy"}) {
			return []byte(phyFixture), nil
		}
		t.Fatalf("unexpected iw argv %v", args)
		return nil, nil
	}
	r, err := s.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || r.GlobalCountry != "AU" || len(r.Phys) != 2 {
		t.Fatalf("snapshot: %v", r)
	}
	if !r.Phys[0].SelfManaged || r.Phys[0].Country != "US" || r.Phys[0].RegulatorySource != "phy" {
		t.Fatalf("self-managed lost: %v", r.Phys[0])
	}
	if r.Phys[1].SelfManaged || r.Phys[1].Country != "AU" || r.Phys[1].RegulatorySource != "global" {
		t.Fatalf("global fallback: %v", r.Phys[1])
	}
	if len(r.Phys[0].Channels) != 4 || r.Phys[0].Channels[0].Constraints != "(22.0 dBm) (no IR)" || r.Phys[0].Channels[3].Constraints != "(disabled)" {
		t.Fatalf("constraints lost: %v", r.Phys[0])
	}
	if r.RawRegulatory != regulatoryFixture || r.Phys[0].RawPhy == "" {
		t.Fatal("raw kernel evidence lost")
	}
}
func TestWiFiCountryRequestIsExplicitHintAndReturnsActualPHY(t *testing.T) {
	s := NewWiFiService(zap.NewNop(), nil)
	calls := [][]string{}
	s.regulatoryCommand = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch len(calls) {
		case 1:
			return nil, nil
		case 2:
			return []byte(regulatoryFixture), nil
		default:
			return []byte(phyFixture), nil
		}
	}
	r, err := s.RequestWiFiCountry(context.Background(), &pb.RequestWiFiCountryRequest{CountryCode: "gb"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"reg", "set", "GB"}, {"reg", "get"}, {"phy"}}) {
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
		if reflect.DeepEqual(args, []string{"reg", "get"}) {
			return []byte(regulatoryFixture), nil
		}
		if reflect.DeepEqual(args, []string{"phy"}) {
			return []byte(phyFixture), nil
		}
		if reflect.DeepEqual(args, []string{"reg", "set", "GB"}) {
			return nil, nil
		}
		t.Fatalf("unexpected command %v", args)
		return nil, nil
	}
	client, close := startWiFiServerWithService(t, s)
	defer close()
	q, err := client.GetWiFiRegulatory(context.Background(), &pb.GetWiFiRegulatoryRequest{})
	if err != nil || q.GlobalCountry != "AU" || q.Phys[0].Country != "US" {
		t.Fatalf("query wire response %v %v", q, err)
	}
	r, err := client.RequestWiFiCountry(context.Background(), &pb.RequestWiFiCountryRequest{CountryCode: "GB"})
	if err != nil || r.RequestedCountry != "GB" || r.Phys[0].Country != "US" {
		t.Fatalf("hint wire response %v %v", r, err)
	}
}
