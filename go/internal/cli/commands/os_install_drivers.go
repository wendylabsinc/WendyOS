//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type preparedInstallDriver struct {
	spec *agentpbv2.DriverSpec
	path string
}

type osInstallDrivers struct {
	deviceType, version, kernel string
	drivers                     []preparedInstallDriver
}

// Resolve and download before flashing. The OS and add-ons remain pinned even
// if the nightly catalog changes while the image is being written.
func prepareOSInstallDrivers(ctx context.Context, deviceType, version string, nightly bool, pr int, names []string) (*osInstallDrivers, error) {
	if len(names) == 0 {
		return nil, nil
	}
	dm, version, err := resolveDeviceArtifactContext(ctx, deviceType, version, nightly, pr)
	if err != nil {
		return nil, err
	}
	v, ok := dm.Versions[version]
	if !ok {
		return nil, fmt.Errorf("no manifest entry for %s on %s", version, deviceType)
	}
	kernel := ""
	if v.Devkit != nil {
		kernel = v.Devkit.KernelVersion
	}
	if kernel == "" {
		for _, e := range v.Extensions {
			if e.KernelVersion == "" {
				continue
			}
			if kernel != "" && kernel != e.KernelVersion {
				return nil, fmt.Errorf("cannot select --driver builds: %s publishes several kernels without a devkit identifying the image kernel", version)
			}
			kernel = e.KernelVersion
		}
	}
	if kernel == "" {
		return nil, fmt.Errorf("%s on %s publishes no driver kernel metadata", version, deviceType)
	}
	p := &osInstallDrivers{deviceType: deviceType, version: version, kernel: kernel}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		e, ok := selectExtension(v.Extensions, name, kernel)
		if !ok {
			p.close()
			return nil, fmt.Errorf("no driver %q published for %s on %s, kernel %s", name, version, deviceType, kernel)
		}
		spec, err := publishedDriverSpec(e)
		if err != nil {
			p.close()
			return nil, err
		}
		cliLogln("Downloading %s for installation after first boot...", name)
		path, err := cachedInstallDriver(ctx, spec)
		if err != nil {
			p.close()
			return nil, err
		}
		spec.ArtifactUrl = ""
		p.drivers = append(p.drivers, preparedInstallDriver{spec: spec, path: path})
	}
	return p, nil
}

// cachedInstallDriver keeps the verified image in the OS cache, named by its
// digest. A sudo re-exec or Windows UAC relaunch replaces this process after
// the download and skips close, so the elevated run reuses and removes it.
func cachedInstallDriver(ctx context.Context, spec *agentpbv2.DriverSpec) (string, error) {
	digest, err := hex.DecodeString(spec.GetSha256())
	if err != nil || len(digest) != sha256.Size {
		return "", fmt.Errorf("driver %q has no valid SHA-256 in the manifest", spec.GetName())
	}
	dir, err := osCacheDir()
	if err != nil {
		return "", err
	}
	want := hex.EncodeToString(digest)
	path := filepath.Join(dir, "driver-"+want+".raw")
	if got, err := sha256File(path); err == nil && got == want {
		return path, nil
	}
	tmp, err := downloadDriver(ctx, spec, dir)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func (p *osInstallDrivers) close() {
	if p == nil {
		return
	}
	for _, d := range p.drivers {
		_ = os.Remove(d.path)
	}
}

// installDriverBootWait bounds the wait for first boot. Each upload afterwards
// has its own driverStageTimeout, so a slow boot cannot starve the uploads.
const installDriverBootWait = 10 * time.Minute

// installedDeviceHostname is the hostname WendyOS derives from a provisioned
// device name (generate-hostname.sh; see maxDeviceNameLen).
func installedDeviceHostname(name string) string {
	return "wendyos-" + name
}

var dialInstallDriverDevice = connectWithAutoTLS

// connectInstallDriverDevice dials the flashed device by its mDNS name, then
// over the USB-C well-known addresses when this host cannot resolve it. The
// USB path only returns a device reporting the same hostname.
func connectInstallDriverDevice(ctx context.Context, host string) (*grpcclient.AgentConnection, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := dialInstallDriverDevice(dialCtx, net.JoinHostPort(host+".local", strconv.Itoa(defaultAgentPort)))
	cancel()
	if err == nil {
		return conn, nil
	}
	if usbConn, ok := usbDirectFallback(ctx, host); ok {
		return usbConn, nil
	}
	return nil, err
}

func (p *osInstallDrivers) installAfterBoot(ctx context.Context, name string) error {
	if p == nil {
		return nil
	}
	host := installedDeviceHostname(name)
	conn, err := p.waitForDevice(ctx, host)
	if err != nil {
		return fmt.Errorf("OS written, but drivers were not installed on %s: %w; install the requested drivers with --device %s.local using the same catalog", host, err, host)
	}
	defer conn.Close()
	for _, d := range p.drivers {
		installCtx, cancel := context.WithTimeout(ctx, driverStageTimeout)
		err := installDriver(installCtx, conn, d.spec, d.path)
		cancel()
		if err != nil {
			return fmt.Errorf("OS written, but installing driver %s after first boot failed: %w", d.spec.GetName(), err)
		}
	}
	cliLogln("Requested drivers installed on %s.", host)
	return nil
}

// Only dial the hostname provisioned by this install, never a default or the
// first discovered board. A matching hostname, board and kernel must precede
// writes.
func (p *osInstallDrivers) waitForDevice(ctx context.Context, host string) (*grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, installDriverBootWait)
	defer cancel()
	cliLogln("Waiting for %s to boot. Keep its USB network cable connected; driver installation will continue automatically.", host)
	var lastErr error
	for {
		conn, err := connectInstallDriverDevice(ctx, host)
		if err == nil {
			checkCtx, stopCheck := context.WithTimeout(ctx, 10*time.Second)
			err = p.checkDevice(checkCtx, conn, host)
			stopCheck()
			if err == nil {
				return conn, nil
			}
			_ = conn.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last check: %v)", ctx.Err(), lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *osInstallDrivers) checkDevice(ctx context.Context, conn *grpcclient.AgentConnection, host string) error {
	v, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return err
	}
	if normalizeMDNSHost(v.GetHostname()) != host || v.GetDeviceType() != p.deviceType {
		return fmt.Errorf("device identity does not match the requested hostname and board")
	}
	dl, err := conn.DriverService.ListDrivers(ctx, &agentpbv2.ListDriversRequest{})
	if err != nil {
		return driverServiceErr(err)
	}
	if dl.GetKernelVersion() != p.kernel {
		return fmt.Errorf("device runs kernel %s; waiting for image kernel %s", dl.GetKernelVersion(), p.kernel)
	}
	return nil
}
