package services

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
)

const (
	// maxWiredAdvertisedAddrs keeps the record inside one 255-byte TXT string.
	maxWiredAdvertisedAddrs = 8
	// wiredAdvertisementInterval paces the loop, which also restores the record
	// after an OTA brings back a fresh service file.
	wiredAdvertisementInterval = 10 * time.Second
	// wiredRemovalSamples is how long, in samples, an advertised address must
	// be gone before it is dropped: a lease can take that long, as behind a
	// spanning-tree switch at boot.
	wiredRemovalSamples = 6
	// wiredRestartRetryTicks spaces out retries of a failed avahi restart.
	wiredRestartRetryTicks = 6
)

const sysClassNet = "/sys/class/net"

var wiredTXTRecordRe = regexp.MustCompile(`\s*<txt-record>wired=([^<]*)</txt-record>`)

type wiredLink struct {
	ethernet bool // a physical Ethernet port: not Wi-Fi, the USB gadget, or virtual
	up       bool
	addrs    []net.IP
}

// RunWiredAdvertisement keeps a wired TXT record naming the device's Ethernet
// addresses, so a host that hears it on both cable and Wi-Fi can prefer the
// cable.
func RunWiredAdvertisement(ctx context.Context, logger *zap.Logger) {
	if _, err := os.Stat(avahiServiceFile); err != nil {
		return
	}
	w := wiredAdvertisement{
		serviceFile:  avahiServiceFile,
		restartAvahi: func() bool { return restartAvahiDaemon(logger) },
	}
	ticker := time.NewTicker(wiredAdvertisementInterval)
	defer ticker.Stop()
	for {
		w.tick(logger, wiredAdvertisedAddrs(listWiredLinks()))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// wiredAdvertisement acts on a change only once it holds across samples:
// avahi withdraws every record while it restarts, so an address missing for a
// moment must not cost a restart.
type wiredAdvertisement struct {
	serviceFile  string
	restartAvahi func() bool

	last    []string
	samples int      // consecutive samples equal to last
	written []string // the record last written to the file
	pending bool     // avahi has not published written yet
	retryIn int      // ticks to wait before retrying a failed restart
}

func (w *wiredAdvertisement) tick(logger *zap.Logger, addrs []string) {
	if w.samples > 0 && slices.Equal(addrs, w.last) {
		w.samples++
	} else {
		w.last, w.samples = addrs, 1
	}
	if w.samples < 2 {
		return
	}
	changed, err := setWiredRecord(w.serviceFile, addrs, w.samples >= wiredRemovalSamples)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Could not update avahi service file", zap.String("path", w.serviceFile), zap.Error(err))
		}
		return
	}
	if changed {
		w.written, w.pending = addrs, true
	}
	if !w.pending {
		return
	}
	if w.retryIn > 0 {
		w.retryIn--
		return
	}
	// Restart, not reload: a reload skips a service file whose mtime is still
	// the second avahi last read it.
	if !w.restartAvahi() {
		w.retryIn = wiredRestartRetryTicks
		return
	}
	w.pending = false
	logger.Info("Advertising wired addresses over mDNS", zap.Strings("addresses", w.written))
}

// setWiredRecord writes addrs into the service file's wired record and reports
// whether the file changed. Unless allowRemoval, it leaves the record alone when
// that would drop an advertised address. The lock covers only the edit, never
// the restart.
func setWiredRecord(serviceFile string, addrs []string, allowRemoval bool) (bool, error) {
	unlock := LockAvahiServiceFile()
	defer unlock()

	data, err := os.ReadFile(serviceFile)
	if err != nil {
		return false, err
	}
	if !allowRemoval {
		if m := wiredTXTRecordRe.FindStringSubmatch(string(data)); m != nil {
			for _, a := range strings.Split(m[1], ",") {
				if !slices.Contains(addrs, a) {
					return false, nil
				}
			}
		}
	}
	content := avahiContentWithWired(string(data), addrs)
	if content == string(data) {
		return false, nil
	}
	return true, os.WriteFile(serviceFile, []byte(content), 0o644)
}

// avahiContentWithWired sets the wired TXT record of the _wendyos._udp block
// to addrs, or removes it when addrs is empty.
func avahiContentWithWired(content string, addrs []string) string {
	return editWendyOSServiceBlock(content, func(block string) string {
		if len(addrs) == 0 {
			return wiredTXTRecordRe.ReplaceAllString(block, "")
		}
		return setAvahiTXTRecord(block, "wired", strings.Join(addrs, ","))
	})
}

// wiredAdvertisedAddrs returns the routable IPv4 addresses of Ethernet ports
// with carrier, in interface order. Link-local addresses name a link, not a
// LAN, and the address the agent takes on a camera port is no way in for a
// host.
func wiredAdvertisedAddrs(links []wiredLink) []string {
	var out []string
	for _, l := range links {
		if !l.ethernet || !l.up {
			continue
		}
		for _, ip := range l.addrs {
			ip4 := ip.To4()
			if ip4 == nil || !ip4.IsGlobalUnicast() { // also drops 169.254/16
				continue
			}
			if n, ok := ipcam.SegmentIndexFor(ip4); ok && ip4.Equal(ipcam.SegmentFor(n).ServerIP) {
				continue
			}
			if len(out) == maxWiredAdvertisedAddrs {
				return out
			}
			out = append(out, ip4.String())
		}
	}
	return out
}

// listWiredLinks reports every interface's addresses and whether it is a live
// Ethernet port.
func listWiredLinks() []wiredLink {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	links := make([]wiredLink, 0, len(ifaces))
	for _, iface := range ifaces {
		l := wiredLink{up: iface.Flags&net.FlagRunning != 0}
		l.ethernet = l.up && isWiredEthernet(filepath.Join(sysClassNet, iface.Name))
		if l.ethernet {
			// Each Addrs call dumps every address on the host; skip it for the
			// rest, such as container veths.
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					l.addrs = append(l.addrs, ipn.IP)
				}
			}
		}
		links = append(links, l)
	}
	return links
}

// isWiredEthernet reports whether a sysfs interface directory describes a
// physical Ethernet port. The USB gadget is the device's end of a cable to one
// host, which ranks that link on its own.
func isWiredEthernet(sysfsDir string) bool {
	arpType, err := os.ReadFile(filepath.Join(sysfsDir, "type"))
	if err != nil || strings.TrimSpace(string(arpType)) != "1" { // ARPHRD_ETHER
		return false
	}
	if _, err := os.Stat(filepath.Join(sysfsDir, "wireless")); err == nil {
		return false
	}
	subsystem, err := os.Readlink(filepath.Join(sysfsDir, "device", "subsystem"))
	if err != nil {
		return false // no backing device: a bridge, veth, or other virtual link
	}
	return filepath.Base(subsystem) != "gadget"
}
