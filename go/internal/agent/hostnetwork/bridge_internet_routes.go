package hostnetwork

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
)

// Before mesh iif routing (18866), NAT replies to an entitled CNI app must
// resolve through main's connected bridge route. This priority is outside
// localmesh's reserved 18865/18866 rules; protocol 202 marks our ownership.
const bridgeReplyPriority = 18863
const bridgeReplyProtocol = "202"
const bridgeGrantDirectory = "/run/wendy/cni/bridge-internet"

type bridgeGrantRecord struct {
	Owner, Bridge string
	IPs           []string
}

func bridgeGrantPath(owner string) string {
	return filepath.Join(bridgeGrantDirectory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(owner))))
}
func rememberBridgeGrant(owner, bridge string, ips []net.IP) error {
	if err := os.MkdirAll(bridgeGrantDirectory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(bridgeGrantDirectory, 0700); err != nil {
		return err
	}
	r := bridgeGrantRecord{Owner: owner, Bridge: bridge}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			r.IPs = append(r.IPs, v4.String())
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(bridgeGrantDirectory, ".grant-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), bridgeGrantPath(owner))
}
func readBridgeGrant(owner string) (*bridgeGrantRecord, error) {
	data, err := os.ReadFile(bridgeGrantPath(owner))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, fmt.Errorf("oversized bridge Internet owner record")
	}
	var r bridgeGrantRecord
	if err = json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.Owner != owner {
		return nil, fmt.Errorf("bridge Internet owner record mismatch")
	}
	var ips []net.IP
	for _, raw := range r.IPs {
		ip := net.ParseIP(raw)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid bridge grant IPv4")
		}
		ips = append(ips, ip)
	}
	if _, err = bridgeInternetRules(r.Owner, r.Bridge, ips); err != nil {
		return nil, err
	}
	return &r, nil
}
func bridgeReplyRulePresent(ip string) (bool, error) {
	out, err := exec.Command("ip", "-j", "-4", "rule", "show").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect bridge reply rules: %w: %s", err, out)
	}
	var rows []map[string]any
	if err = json.Unmarshal(out, &rows); err != nil {
		return false, err
	}
	for _, row := range rows {
		if row["priority"] != float64(bridgeReplyPriority) || row["dst"] != ip {
			if row["priority"] != float64(bridgeReplyPriority) || row["dst"] != ip+"/32" {
				continue
			}
		}
		for key, value := range row {
			switch key {
			case "priority", "dst", "table", "protocol":
			case "src":
				if value != "all" {
					return false, fmt.Errorf("unexpected bridge reply source selector")
				}
			default:
				return false, fmt.Errorf("unexpected bridge reply rule selector %s", key)
			}
		}
		table := fmt.Sprint(row["table"])
		if (table != "main" && table != "254") || fmt.Sprint(row["protocol"]) != bridgeReplyProtocol {
			return false, fmt.Errorf("bridge reply rule ownership conflict for %s", ip)
		}
		return true, nil
	}
	return false, nil
}
func ensureBridgeReplyRule(ip string) error {
	exists, err := bridgeReplyRulePresent(ip)
	if err != nil || exists {
		return err
	}
	out, err := exec.Command("ip", "-4", "rule", "add", "pref", fmt.Sprint(bridgeReplyPriority), "to", ip+"/32", "lookup", "main", "protocol", bridgeReplyProtocol).CombinedOutput()
	if err != nil {
		return fmt.Errorf("install bridge reply rule: %w: %s", err, out)
	}
	return nil
}
func revokeBridgeReplyRules(owner string) error {
	r, err := readBridgeGrant(owner)
	if err != nil || r == nil {
		return err
	}
	for _, ip := range r.IPs {
		exists, err := bridgeReplyRulePresent(ip)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		out, err := exec.Command("ip", "-4", "rule", "del", "pref", fmt.Sprint(bridgeReplyPriority), "to", ip+"/32", "lookup", "main", "protocol", bridgeReplyProtocol).CombinedOutput()
		if err != nil {
			return fmt.Errorf("revoke bridge reply rule: %w: %s", err, out)
		}
	}
	if err := os.Remove(bridgeGrantPath(owner)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
