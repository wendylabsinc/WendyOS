//go:build linux

package localmesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// HostPolicy owns resolver roaming only. Internet tunnels, leases and forwarding
// policy belong to meshsharing; Babel owns device ULA routing.
type HostPolicy struct {
	dnsAddress, dnsInterface, previousResolver string
	resolverFiles                              *resolverFiles
}

type resolverFiles struct{ link, stub, journal string }

func (p *HostPolicy) files() resolverFiles {
	if p.resolverFiles != nil {
		return *p.resolverFiles
	}
	return resolverFiles{"/etc/resolv.conf", resolverStub, "/etc/.wendy-local-mesh-resolver.json"}
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

// DNS is associated with the real TUN toward the gateway, not the dummy address
// interface: resolved binds its outbound queries to this link.
func (p *HostPolicy) SetDNS(ctx context.Context, iface, address string) error {
	if address == "" && p.dnsInterface == "" {
		return p.restoreResolver()
	}
	if address == p.dnsAddress && iface == p.dnsInterface {
		return nil
	}
	if err := p.revertDNS(ctx); err != nil {
		return err
	}
	if address == "" {
		return p.restoreResolver()
	}
	// Keep partial applies out of the unchanged-policy fast path until cleanup
	// succeeds, including a failed command that may already have applied DNS.
	p.dnsInterface = iface
	if _, err := policyCommand(ctx, "resolvectl", "dns", iface, address); err != nil {
		return err
	}
	if _, err := policyCommand(ctx, "resolvectl", "domain", iface, "~."); err != nil {
		return errors.Join(err, p.revertDNS(ctx))
	}
	if err := p.useResolverStub(); err != nil {
		return errors.Join(err, p.revertDNS(ctx))
	}
	p.dnsAddress = address
	return nil
}

func (p *HostPolicy) revertDNS(ctx context.Context) error {
	if p.dnsInterface == "" {
		return nil
	}
	if _, err := policyCommand(ctx, "resolvectl", "revert", p.dnsInterface); err != nil {
		if _, lookup := net.InterfaceByName(p.dnsInterface); lookup == nil {
			return err
		}
	}
	p.dnsInterface, p.dnsAddress = "", ""
	return nil
}

func (p *HostPolicy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return p.SetDNS(ctx, "", "")
}

const resolverStub = "/run/systemd/resolve/stub-resolv.conf"

func replaceResolverLink(path, target string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".wendy-local-mesh-resolver-")
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
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncResolverDirectory(path)
}

func syncResolverDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type resolverJournal struct {
	Version  int    `json:"version"`
	Previous string `json:"previous"`
}

// CleanupStaleResolver restores only a symlink recorded before mesh changed it.
// The journal survives process exit so failed reset cleanup can retry at startup.
func CleanupStaleResolver(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return (&HostPolicy{}).restoreResolver()
}

func (p *HostPolicy) useResolverStub() error {
	f := p.files()
	if p.previousResolver == "" {
		if err := p.restoreResolver(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(f.stub); err != nil {
		return err
	}
	current, err := os.Readlink(f.link)
	if err != nil {
		return fmt.Errorf("wendy net DNS requires a managed resolv.conf symlink: %w", err)
	}
	resolved := current
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(f.link), resolved)
	}
	if filepath.Clean(resolved) == f.stub {
		return nil
	}
	if p.previousResolver != "" {
		return errors.New("resolver changed externally during wendy net roaming")
	}
	data, err := json.Marshal(resolverJournal{Version: 1, Previous: current})
	if err != nil {
		return err
	}
	// Persist ownership before changing the link. Recovery accepts either the
	// original link or our stub, covering a crash on either side of the rename.
	if err = atomicfile.Write(f.journal, data, 0600); err != nil {
		return err
	}
	if err = syncResolverDirectory(f.journal); err != nil {
		return err
	}
	p.previousResolver = current
	if err = replaceResolverLink(f.link, f.stub); err != nil {
		return err
	}
	return nil
}

func (p *HostPolicy) restoreResolver() error {
	f := p.files()
	if p.previousResolver == "" {
		data, err := os.ReadFile(f.journal)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var saved resolverJournal
		if err = json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("invalid net resolver journal: %w", err)
		}
		if saved.Version != 1 || saved.Previous == "" || saved.Previous == f.stub {
			return errors.New("invalid net resolver ownership journal")
		}
		p.previousResolver = saved.Previous
	}
	current, err := os.Readlink(f.link)
	if err != nil {
		return err
	}
	if current != f.stub && current != p.previousResolver {
		return errors.New("resolver ownership changed; refusing overwrite")
	}
	if current != p.previousResolver {
		if err = replaceResolverLink(f.link, p.previousResolver); err != nil {
			return err
		}
	}
	// A prior rename may have succeeded while its directory sync failed.
	// Confirm the restored link is durable before discarding its ownership.
	if err = syncResolverDirectory(f.link); err != nil {
		return err
	}
	if err = os.Remove(f.journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = syncResolverDirectory(f.journal); err != nil {
		return err
	}
	p.previousResolver = ""
	return nil
}
