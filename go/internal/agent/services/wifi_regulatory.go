package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/wifiregulatory"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const regulatoryDetail = "Global country is a kernel hint. Self-managed PHY domains may differ and cannot be overridden by this request. Effective channel flags, bandwidth, DFS and power limits remain authoritative. Requests are runtime only and may be changed by drivers or later hints."

func (s *WiFiService) runRegulatory(ctx context.Context, args ...string) ([]byte, error) {
	if s.regulatoryCommand != nil {
		return s.regulatoryCommand(ctx, args...)
	}
	if runtime.GOOS != "linux" {
		return nil, status.Error(codes.Unimplemented, "Wi-Fi regulatory controls require Linux")
	}
	binary, err := exec.LookPath("iw")
	if err != nil {
		return nil, status.Error(codes.Unavailable, "Wi-Fi regulatory controls require iw on the device")
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	out, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("iw %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (s *WiFiService) GetWiFiRegulatory(ctx context.Context, _ *pb.GetWiFiRegulatoryRequest) (*pb.WiFiRegulatoryStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.regulatorySnapshot(ctx)
}

func (s *WiFiService) RequestWiFiCountry(ctx context.Context, req *pb.RequestWiFiCountryRequest) (*pb.WiFiRegulatoryStatus, error) {
	country, err := wifiregulatory.Country(req.GetCountryCode())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.runRegulatory(ctx, "reg", "set", country); err != nil {
		return nil, regulatoryError(ctx, err)
	}
	snapshot, err := s.regulatorySnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "country hint %s submitted; reading resulting regulatory state failed: %v", country, err)
	}
	snapshot.RequestedCountry = country
	snapshot.Detail = "Country hint submitted; kernel processing may be asynchronous. " + regulatoryDetail
	return snapshot, nil
}

func regulatoryError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.Unavailable, err.Error())
}

func (s *WiFiService) regulatorySnapshot(ctx context.Context) (*pb.WiFiRegulatoryStatus, error) {
	reg, err := s.runRegulatory(ctx, "reg", "get")
	if err != nil {
		return nil, regulatoryError(ctx, err)
	}
	phy, err := s.runRegulatory(ctx, "phy")
	if err != nil {
		return nil, regulatoryError(ctx, err)
	}
	snapshot, err := parseWiFiRegulatory(string(reg), string(phy))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "cannot interpret kernel regulatory state: %v", err)
	}
	return snapshot, nil
}

var regPhyHeader = regexp.MustCompile(`^phy#([0-9]+)(?:\s+\((self-managed)\))?\s*$`)
var regCountry = regexp.MustCompile(`^country ([A-Za-z0-9]{2}):`)
var phyHeader = regexp.MustCompile(`^Wiphy (\S+)\s*$`)
var phyIndex = regexp.MustCompile(`^wiphy index: ([0-9]+)$`)
var channelLine = regexp.MustCompile(`^\* ([0-9]+(?:\.[0-9]+)?) MHz \[([0-9]+)\](.*)$`)

func parseWiFiRegulatory(reg, phy string) (*pb.WiFiRegulatoryStatus, error) {
	result := &pb.WiFiRegulatoryStatus{RawRegulatory: reg, Detail: regulatoryDetail}
	domains := map[string]*pb.WiFiRegulatoryPhy{}
	current := ""
	for _, line := range strings.Split(reg, "\n") {
		line = strings.TrimSpace(line)
		if line == "global" {
			current = "global"
			continue
		}
		if match := regPhyHeader.FindStringSubmatch(line); match != nil {
			current = "phy" + match[1]
			domains[current] = &pb.WiFiRegulatoryPhy{Name: current, SelfManaged: match[2] != "", RegulatorySource: "phy"}
			continue
		}
		if match := regCountry.FindStringSubmatch(line); match != nil {
			if current == "global" {
				result.GlobalCountry = match[1]
			} else if domain := domains[current]; domain != nil {
				domain.Country = match[1]
			}
		}
	}
	if result.GlobalCountry == "" && len(domains) == 0 {
		return nil, fmt.Errorf("no global or PHY regulatory domain reported")
	}
	var device *pb.WiFiRegulatoryPhy
	indexes := map[*pb.WiFiRegulatoryPhy]string{}
	for _, line := range strings.Split(phy, "\n") {
		trimmed := strings.TrimSpace(line)
		if match := phyHeader.FindStringSubmatch(trimmed); match != nil {
			device = &pb.WiFiRegulatoryPhy{Name: match[1]}
			if strings.HasPrefix(match[1], "phy") {
				indexes[device] = match[1]
			}
			result.Phys = append(result.Phys, device)
		}
		if device == nil {
			continue
		}
		device.RawPhy += line + "\n"
		if match := phyIndex.FindStringSubmatch(trimmed); match != nil {
			indexes[device] = "phy" + match[1]
		}
		if match := channelLine.FindStringSubmatch(trimmed); match != nil {
			frequency, err := strconv.ParseFloat(match[1], 64)
			if err != nil {
				return nil, err
			}
			device.Channels = append(device.Channels, &pb.WiFiRegulatoryChannel{FrequencyMhz: frequency, Constraints: strings.TrimSpace(match[3])})
		}
	}
	if strings.TrimSpace(phy) != "" && len(result.Phys) == 0 {
		return nil, fmt.Errorf("no PHY inventory headers reported")
	}
	seen := map[string]bool{}
	for _, device := range result.Phys {
		key := indexes[device]
		if key == "" {
			return nil, fmt.Errorf("%s PHY has no index", device.Name)
		}
		if domain := domains[key]; domain != nil {
			device.Country, device.SelfManaged, device.RegulatorySource = domain.Country, domain.SelfManaged, "phy"
			seen[key] = true
		} else {
			device.Country, device.RegulatorySource = result.GlobalCountry, "global"
		}
	}
	for name, domain := range domains {
		if domain.Country == "" {
			return nil, fmt.Errorf("%s domain has no country", name)
		}
		if !seen[name] {
			return nil, fmt.Errorf("%s regulatory domain missing from PHY inventory", name)
		}
	}
	return result, nil
}
