package agentscope

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const scopeAgentPort = 21051

func admitScopePorts(ephemeral string, port int) error {
	fields := strings.Fields(ephemeral)
	if len(fields) != 2 {
		return fmt.Errorf("invalid kernel ephemeral port range %q", ephemeral)
	}
	low, lowErr := strconv.Atoi(fields[0])
	high, highErr := strconv.Atoi(fields[1])
	if lowErr != nil || highErr != nil || low < 1 || high > 65535 || low > high {
		return fmt.Errorf("invalid kernel ephemeral port range %q", ephemeral)
	}
	if port < 1024 || port+1 >= low {
		return fmt.Errorf("startup ports %d/%d must be unprivileged and below kernel ephemeral range %d-%d", port, port+1, low, high)
	}
	return nil
}

func TestScopeStartupPortAdmission(t *testing.T) {
	for _, test := range []struct {
		name, ephemeral string
		port            int
		accepted        bool
	}{
		{"current Linux defaults", "32768\t60999\n", scopeAgentPort, true},
		{"historical probe ports overlap ephemeral sources", "32768 60999", 51051, false},
		{"custom low ephemeral range", "20000 60999", scopeAgentPort, false},
		{"mTLS at lower boundary", "21052 60999", scopeAgentPort, false},
		{"both immediately below boundary", "21053 60999", scopeAgentPort, true},
		{"privileged setup port", "32768 60999", 1023, false},
		{"missing range", "", scopeAgentPort, false},
		{"non numeric", "default 60999", scopeAgentPort, false},
		{"reversed range", "60999 32768", scopeAgentPort, false},
		{"out of bounds", "32768 65536", scopeAgentPort, false},
		{"unexpected extra value", "32768 60999 1", scopeAgentPort, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := admitScopePorts(test.ephemeral, test.port); (err == nil) != test.accepted {
				t.Fatalf("accepted=%v error=%v", test.accepted, err)
			}
		})
	}
}
