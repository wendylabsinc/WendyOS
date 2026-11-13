package hostnetwork

import (
	"net"
	"slices"
	"strings"
	"testing"
)

func TestBridgeInternetGrantTargetsInnerTunnelAndKeepsIsolation(t *testing.T) {
	rules, err := bridgeInternetRules("test-owner", "app0", []net.IP{net.ParseIP("10.42.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	arg := func(rule []string, flag string) string {
		i := slices.Index(rule, flag)
		if i < 0 || i+1 == len(rule) {
			return ""
		}
		return rule[i+1]
	}
	seenExclusions := map[string]bool{}
	forward, reply := false, false
	for _, rule := range rules {
		if slices.Contains(rule, "wlmp+") {
			if arg(rule, "-j") != "RETURN" {
				t.Fatalf("grant permits raw forwarding onto a Babel peer: %v", rule)
			}
		}
		if arg(rule, "-o") != "wlmi+" && arg(rule, "-i") != "wlmi+" {
			continue // Native grants have a separate destination/reply contract.
		}
		if arg(rule, "-i") == "app0" {
			if arg(rule, "-o") != "wlmi+" || arg(rule, "-s") != "10.42.0.2/32" {
				t.Fatalf("grant escaped the app address or Internet tunnel: %v", rule)
			}
			if arg(rule, "-j") == "RETURN" {
				seenExclusions[arg(rule, "-d")] = true
			} else if arg(rule, "-j") == "ACCEPT" {
				forward = true
			}
		} else if arg(rule, "-j") == "ACCEPT" {
			if arg(rule, "-i") != "wlmi+" || arg(rule, "-o") != "app0" ||
				arg(rule, "-d") != "10.42.0.2/32" || arg(rule, "--ctstate") != "ESTABLISHED,RELATED" {
				t.Fatalf("unsolicited tunnel traffic can reach the app: %v", rule)
			}
			reply = true
		}
	}
	if !forward || !reply {
		t.Fatalf("missing outbound or established reply grant: %v", rules)
	}
	for _, prefix := range []string{"10.88.0.0/16", "10.99.0.0/16", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"} {
		if !seenExclusions[prefix] {
			t.Errorf("Internet grant exposes reserved destination %s", prefix)
		}
	}
}

func TestNativeBridgeInternetGrantRequiresExactAppAndTrackedReply(t *testing.T) {
	rules, err := bridgeInternetRules("test-owner", "app0", []net.IP{net.ParseIP("10.42.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	var grants [][]string
	for _, rule := range rules {
		if slices.Contains(rule, "wlmi+") || !slices.Contains(rule, "ACCEPT") {
			continue
		}
		grants = append(grants, rule)
	}
	if len(grants) != 2 {
		t.Fatalf("want one native outbound/reply pair, got %v", grants)
	}
	for i, fields := range [][]string{
		{"-i app0", "-s 10.42.0.2/32", "--ctstate NEW,ESTABLISHED", "--ctdir ORIGINAL"},
		{"-o app0", "-d 10.42.0.2/32", "--ctstate ESTABLISHED,RELATED", "--ctdir REPLY"},
	} {
		text := strings.Join(grants[i], " ")
		for _, field := range fields {
			if !strings.Contains(text, field) {
				t.Fatalf("native grant misses %s: %s", field, text)
			}
		}
	}
}
