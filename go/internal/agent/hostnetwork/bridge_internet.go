package hostnetwork

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"
)

// BridgeInternetChain contains CNI-owned grants for explicit Internet-enabled
// bridge apps. Local mesh policy calls it before its forwarding fallback.
const BridgeInternetChain = "WENDY-BRIDGE-INTERNET"

func InitBridgeInternetChain() error { return ensureChain(BridgeInternetChain) }
func bridgeInternetMarker(owner string) string {
	return fmt.Sprintf("wendy-bridge-internet:%x", sha256.Sum256([]byte(owner)))
}
func bridgeInternetRules(owner, bridge string, ips []net.IP) ([][]string, error) {
	if owner == "" || !lanInterfaceName.MatchString(bridge) {
		return nil, fmt.Errorf("invalid bridge Internet grant")
	}
	var rules [][]string
	marker := bridgeInternetMarker(owner)
	for _, raw := range ips {
		ip := raw.To4()
		if ip == nil {
			continue
		}
		if !ip.IsGlobalUnicast() {
			return nil, fmt.Errorf("invalid bridge Internet address")
		}
		source := ip.String() + "/32"
		for _, dst := range []string{"10.88.0.0/16", "10.99.0.0/16", "0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
			rules = append(rules, []string{"-i", bridge, "-o", "wlmp+", "-s", source, "-d", dst, "-m", "comment", "--comment", marker, "-j", "RETURN"})
		}
		rules = append(rules, []string{"-i", bridge, "-o", "wlmp+", "-s", source, "-m", "comment", "--comment", marker, "-j", "ACCEPT"})
		rules = append(rules, []string{"-i", "wlmp+", "-o", bridge, "-d", source, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-m", "comment", "--comment", marker, "-j", "ACCEPT"})
	}
	return rules, nil
}

// RevokeBridgeInternetAccess works without a surviving namespace or address.
// Delete only this CNI invocation's exact marker; unrelated grants stay intact.
func RevokeBridgeInternetAccess(owner string) error {
	out, err := meshIPTables("-t", "filter", "-S", BridgeInternetChain)
	if err != nil {
		if exitCode(err) == 1 {
			return nil
		}
		return fmt.Errorf("inspect bridge Internet grants: %w: %s", err, out)
	}
	marker := bridgeInternetMarker(owner)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != BridgeInternetChain {
			continue
		}
		for i := range fields {
			fields[i] = strings.Trim(fields[i], "\"")
		}
		owned := false
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "--comment" && fields[i+1] == marker {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		// Match the full rule instead of a line number: another CNI process may
		// delete an earlier sibling rule between this observation and deletion.
		if output, e := meshIPTables(append([]string{"-t", "filter", "-D", BridgeInternetChain}, fields[2:]...)...); e != nil && exitCode(e) != 1 {
			return fmt.Errorf("revoke bridge Internet grant: %w: %s", e, output)
		}
	}
	return nil
}
func EnsureBridgeInternetAccess(owner, bridge string, ips []net.IP) (err error) {
	rules, err := bridgeInternetRules(owner, bridge, ips)
	if err != nil {
		return err
	}
	if err = InitBridgeInternetChain(); err != nil {
		return err
	}
	if err = RevokeBridgeInternetAccess(owner); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = RevokeBridgeInternetAccess(owner)
		}
	}()
	for _, rule := range rules {
		if out, e := meshIPTables(append([]string{"-t", "filter", "-A", BridgeInternetChain}, rule...)...); e != nil {
			return fmt.Errorf("grant bridge Internet access: %w: %s", e, out)
		}
	}
	return nil
}
func CheckBridgeInternetAccess(owner, bridge string, ips []net.IP) error {
	rules, err := bridgeInternetRules(owner, bridge, ips)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if out, e := meshIPTables(append([]string{"-t", "filter", "-C", BridgeInternetChain}, rule...)...); e != nil {
			return fmt.Errorf("bridge Internet grant missing: %w: %s", e, out)
		}
	}
	return nil
}
