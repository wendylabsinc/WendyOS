package containerd

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestROS2ExternalIPv4Admission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   net.Flags
		address string
		want    bool
	}{
		{"loopback", net.FlagUp | net.FlagLoopback, "127.0.0.1", false},
		{"down-addressed", 0, "192.0.2.10", false},
		{"unaddressed", net.FlagUp, "", false},
		{"unspecified", net.FlagUp, "0.0.0.0", false},
		{"ipv6-only", net.FlagUp, "2001:db8::10", false},
		{"ipv4", net.FlagUp, "192.0.2.10", true},
		{"ipv4-linklocal", net.FlagUp, "169.254.3.2", true},
		{"point-to-point", net.FlagUp | net.FlagPointToPoint, "192.0.2.10", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ros2ExternalIPv4([]net.Interface{{Name: "candidate", Flags: tc.flags}}, func(net.Interface) ([]net.Addr, error) {
				if tc.address == "" {
					return nil, nil
				}
				return []net.Addr{&net.IPNet{IP: net.ParseIP(tc.address)}}, nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("eligible=%v error=%v want=%v", got, err, tc.want)
			}
		})
	}
	want := errors.New("interface address observation unavailable")
	if _, err := ros2ExternalIPv4([]net.Interface{{Flags: net.FlagUp}}, func(net.Interface) ([]net.Addr, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("observation failure lost: %v", err)
	}
}

func TestBuildROS2NetworkDoesNotSelectLoopbackTwice(t *testing.T) {
	prior := ros2HasExternalIPv4
	defer func() { ros2HasExternalIPv4 = prior }()
	cfg := &appconfig.AppConfig{Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{Scope: "network"}}}
	for _, external := range []bool{false, true} {
		ros2HasExternalIPv4 = func() (bool, error) { return external, nil }
		env := mustBuildROS2Env(t, cfg, "test.network", "")
		xml := lastEnv(env, "CYCLONEDDS_URI")
		if strings.Count(xml, `name="lo"`) != 1 || strings.Contains(xml, `autodetermine="true"`) != external {
			t.Fatalf("NETWORK_LOOPBACK_SELECTION external=%v xml=%s", external, xml)
		}
		for _, want := range []string{`<Peer address="127.0.0.1"/>`, `<ParticipantIndex>auto</ParticipantIndex>`, `<Enable>false</Enable>`} {
			if !strings.Contains(xml, want) {
				t.Fatalf("lost device-app discovery %s: %s", want, xml)
			}
		}
		if lastEnv(env, "ROS_LOCALHOST_ONLY") != "0" || lastEnv(env, "ROS_AUTOMATIC_DISCOVERY_RANGE") != "SUBNET" {
			t.Fatalf("network entitlement changed: %v", env)
		}
	}
	ros2HasExternalIPv4 = func() (bool, error) { return false, errors.New("unavailable") }
	if _, err := buildROS2Env(cfg, "test.network", ""); err == nil {
		t.Fatal("interface observation error silently changed network selection")
	}
}
