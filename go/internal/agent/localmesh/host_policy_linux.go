//go:build linux

package localmesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type policyRule struct {
	table, chain string
	args         []string
}

// HostPolicy owns only scoped forwarding/NAT/DNS changes. Babel/Node owns route
// selection, including removal of host roaming defaults when a route disappears.
type HostPolicy struct {
	asset                                      int32
	base, share                                []policyRule
	previousForward                            string
	forwardChanged                             bool
	dns                                        *exec.Cmd
	dnsDone                                    chan error
	sharing                                    string
	dnsAddress, dnsInterface, previousResolver string
}

func policyCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return out, nil
}

func (p *HostPolicy) add(ctx context.Context, share bool, table, chain string, args ...string) error {
	args = append(args, "-m", "comment", "--comment", "wendy-local-mesh-v1")
	command := append([]string{"-w", "3", "-t", table, "-I", chain, "1"}, args...)
	if _, err := policyCommand(ctx, "iptables", command...); err != nil {
		return err
	}
	r := policyRule{table, chain, args}
	if share {
		p.share = append(p.share, r)
	} else {
		p.base = append(p.base, r)
	}
	return nil
}

func (p *HostPolicy) remove(ctx context.Context, rules []policyRule) error {
	var errs []error
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		_, err := policyCommand(ctx, "iptables", append([]string{"-w", "3", "-t", r.table, "-D", r.chain}, r.args...)...)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func NewHostPolicy(ctx context.Context, asset int32) (p *HostPolicy, err error) {
	p = &HostPolicy{asset: asset}
	defer func() {
		if err != nil && p != nil {
			_ = p.Close()
		}
	}()
	previous, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return nil, err
	}
	p.previousForward = string(previous)
	add := func(args ...string) error { return p.add(ctx, false, "filter", "FORWARD", args...) }
	if strings.TrimSpace(p.previousForward) != "1" {
		if err = add("-j", "DROP"); err != nil {
			return p, err
		}
	}
	if err = add("-i", "wlmp+", "-j", "DROP"); err != nil {
		return p, err
	}
	if err = add("-o", "wlmp+", "-j", "DROP"); err != nil {
		return p, err
	}
	if err = add("-i", "wlmp+", "-o", "wlmp+", "-s", "10.88.0.0/16", "-j", "ACCEPT"); err != nil {
		return p, err
	}
	if err = add("-i", "wlmp+", "-o", "wlmp+", "-d", "10.88.0.0/16", "-j", "ACCEPT"); err != nil {
		return p, err
	}
	for _, subnet := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
		if err = add("-i", "wlmp+", "-d", subnet, "-j", "REJECT"); err != nil {
			return p, err
		}
	}
	if strings.TrimSpace(p.previousForward) != "1" {
		if err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0600); err != nil {
			return p, err
		}
		p.forwardChanged = true
	}
	return p, nil
}

func (p *HostPolicy) stopSharing(ctx context.Context) error {
	if p.dns != nil {
		_ = p.dns.Process.Kill()
		<-p.dnsDone
		p.dns = nil
		p.dnsDone = nil
	}
	err := p.remove(ctx, p.share)
	p.share = nil
	p.sharing = ""
	return err
}

func (p *HostPolicy) SetSharing(ctx context.Context, iface, dns string) (err error) {
	desired := ""
	if iface != "" {
		desired = iface + "|" + dns
	}
	if p.dnsDone != nil {
		select {
		case exit := <-p.dnsDone:
			p.dnsDone <- exit
			_ = p.stopSharing(ctx)
			return fmt.Errorf("mesh DNS proxy stopped: %v", exit)
		default:
		}
	}
	if desired == p.sharing {
		return nil
	}
	if err = p.stopSharing(ctx); err != nil {
		return err
	}
	if desired == "" {
		return nil
	}
	if net.ParseIP(dns) == nil {
		return errors.New("sharing requires upstream DNS")
	}
	defer func() {
		if err != nil {
			_ = p.stopSharing(context.Background())
		}
	}()
	if err = p.add(ctx, true, "nat", "POSTROUTING", "-s", "10.88.0.0/16", "-o", iface, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if err = p.add(ctx, true, "filter", "FORWARD", "-i", "wlmp+", "-o", iface, "-s", "10.88.0.0/16", "-j", "ACCEPT"); err != nil {
		return err
	}
	for _, subnet := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
		if err = p.add(ctx, true, "filter", "FORWARD", "-i", "wlmp+", "-o", iface, "-d", subnet, "-j", "REJECT"); err != nil {
			return err
		}
	}
	if err = p.add(ctx, true, "filter", "FORWARD", "-i", iface, "-o", "wlmp+", "-d", "10.88.0.0/16", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	ip, _, _ := Addresses(1, p.asset)
	for _, proto := range []string{"udp", "tcp"} {
		if err = p.add(ctx, true, "filter", "INPUT", "!", "-i", "wlmp+", "-d", ip.String(), "-p", proto, "--dport", "53", "-j", "DROP"); err != nil {
			return err
		}
	}
	args := []string{"--conf-file=/dev/null", "--keep-in-foreground", "--no-resolv", "--no-hosts", "--bind-interfaces", "--listen-address=" + ip.String(), "--server=" + dns + "@" + iface, "--cache-size=150", "--pid-file="}
	if _, err = policyCommand(ctx, "dnsmasq", append(args, "--test")...); err != nil {
		return err
	}
	p.dns = exec.Command("dnsmasq", args...)
	if err = p.dns.Start(); err != nil {
		p.dns = nil
		return err
	}
	p.dnsDone = make(chan error, 1)
	cmd, done := p.dns, p.dnsDone
	go func() { done <- cmd.Wait() }()
	select {
	case exit := <-done:
		done <- exit
		return fmt.Errorf("mesh DNS failed to start: %v", exit)
	case <-time.After(150 * time.Millisecond):
	}
	p.sharing = desired
	return nil
}

// DNS is associated with the real TUN toward the gateway, not the dummy address
// interface: resolved binds its outbound queries to this link.
func (p *HostPolicy) SetDNS(ctx context.Context, iface, address string) error {
	if address == p.dnsAddress && iface == p.dnsInterface {
		return nil
	}
	if p.dnsInterface != "" {
		_, err := policyCommand(ctx, "resolvectl", "revert", p.dnsInterface)
		if err != nil {
			if _, lookup := net.InterfaceByName(p.dnsInterface); lookup == nil {
				return err
			}
		}
		p.dnsInterface = ""
		p.dnsAddress = ""
	}
	if address == "" {
		return p.restoreResolver()
	}
	if _, err := policyCommand(ctx, "resolvectl", "dns", iface, address); err != nil {
		return err
	}
	if _, err := policyCommand(ctx, "resolvectl", "domain", iface, "~."); err != nil {
		_, _ = policyCommand(ctx, "resolvectl", "revert", iface)
		return err
	}
	if err := p.useResolverStub(); err != nil {
		_, _ = policyCommand(ctx, "resolvectl", "revert", iface)
		return err
	}
	p.dnsInterface = iface
	p.dnsAddress = address
	return nil
}

func (p *HostPolicy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var errs []error
	errs = append(errs, p.SetDNS(ctx, "", ""), p.stopSharing(ctx))
	if p.forwardChanged {
		current, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err == nil && strings.TrimSpace(string(current)) == "1" {
			err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(p.previousForward), 0600)
		}
		errs = append(errs, err)
		p.forwardChanged = false
	}
	errs = append(errs, p.remove(ctx, p.base))
	p.base = nil
	return errors.Join(errs...)
}

const resolverStub = "/run/systemd/resolve/stub-resolv.conf"

func replaceResolverLink(target string) error {
	file, err := os.CreateTemp("/etc", ".wendy-local-mesh-resolver-")
	if err != nil {
		return err
	}
	name := file.Name()
	_ = file.Close()
	defer os.Remove(name)
	if err = os.Remove(name); err != nil {
		return err
	}
	if err = os.Symlink(target, name); err != nil {
		return err
	}
	return os.Rename(name, "/etc/resolv.conf")
}
func (p *HostPolicy) useResolverStub() error {
	if _, err := os.Stat(resolverStub); err != nil {
		return err
	}
	current, err := os.Readlink("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("mesh DNS requires a managed resolv.conf symlink: %w", err)
	}
	resolved := current
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join("/etc", resolved)
	}
	if filepath.Clean(resolved) == resolverStub {
		return nil
	}
	if p.previousResolver != "" {
		return errors.New("resolver changed externally during local-mesh roaming")
	}
	if err = replaceResolverLink(resolverStub); err != nil {
		return err
	}
	p.previousResolver = current
	return nil
}
func (p *HostPolicy) restoreResolver() error {
	if p.previousResolver == "" {
		return nil
	}
	current, err := os.Readlink("/etc/resolv.conf")
	if err != nil {
		return err
	}
	if current != resolverStub {
		return errors.New("resolver ownership changed; refusing overwrite")
	}
	if err = replaceResolverLink(p.previousResolver); err != nil {
		return err
	}
	p.previousResolver = ""
	return nil
}
