package unitree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"golang.org/x/net/ipv4"
)

const discoveryTimeout = 3 * time.Second

// Stock firmware sends discovery replies to one fixed UDP port. Only one
// in-process scan may own that port; waiting callers remain cancellable.
var discoverySlot = make(chan struct{}, 1)

// Discover scans the machine's LAN. Stock Go2 firmware answers Unitree UDP
// multicast, not DNS-SD. We also browse common mDNS services, accepting them
// only when their advertised identity matches a known account serial number.
// Results are candidates; Connect must verify them before saving a pairing.
func Discover(ctx context.Context, knownSerials ...string) ([]Device, error) {
	return discoverSerialized(ctx, knownSerials, discoverMulticast, discovery.BrowseMDNSServices)
}

func discoverSerialized(ctx context.Context, serials []string, multicast multicastScan, browse mdnsBrowse) ([]Device, error) {
	select {
	case discoverySlot <- struct{}{}:
		defer func() { <-discoverySlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return discover(ctx, serials, multicast, browse)
}

type multicastScan func(context.Context, []string) ([]Device, error)
type mdnsBrowse func(context.Context, string, time.Duration) ([]discovery.MDNSService, error)

func discover(ctx context.Context, serials []string, multicast multicastScan, browse mdnsBrowse) ([]Device, error) {
	if len(serials) > 256 {
		return nil, errors.New("at most 256 Unitree serial numbers can be scanned at once")
	}
	for _, serial := range serials {
		if !ValidSerial(serial) {
			return nil, errors.New("invalid Unitree serial number")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scanCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	type result struct {
		devices []Device
		err     error
		udp     bool
	}
	results := make(chan result, 3)
	var workers sync.WaitGroup
	workers.Go(func() {
		devices, err := multicast(scanCtx, serials)
		results <- result{devices: devices, err: err, udp: true}
	})
	if len(serials) > 0 {
		for _, service := range []string{"_ssh._tcp", "_http._tcp"} {
			workers.Go(func() {
				entries, err := browse(scanCtx, service, discoveryTimeout)
				var devices []Device
				for _, entry := range entries {
					if device, ok := deviceFromMDNS(entry, serials); ok {
						devices = append(devices, device)
					}
				}
				results <- result{devices: devices, err: err}
			})
		}
	}
	workers.Wait()
	close(results)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	devices := make(map[string]Device)
	var udpErr error
	for result := range results {
		if result.udp {
			udpErr = result.err
		}
		for _, device := range result.devices {
			// Prefer Unitree's own discovery over a generic mDNS announcement.
			if _, exists := devices[device.Serial]; !exists || result.udp {
				devices[device.Serial] = device
			}
		}
	}
	if len(devices) == 0 && udpErr != nil && !errors.Is(udpErr, context.DeadlineExceeded) {
		return nil, fmt.Errorf("scanning for Unitree robots: %w; enter the robot's IP address instead", udpErr)
	}
	list := make([]Device, 0, len(devices))
	for _, device := range devices {
		list = append(list, device)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Serial < list[j].Serial })
	return list, nil
}

func discoverMulticast(ctx context.Context, serials []string) ([]Device, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 10134})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	packet := ipv4.NewPacketConn(conn)
	if err := packet.SetMulticastTTL(1); err != nil {
		return nil, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	group := &net.UDPAddr{IP: net.IPv4(231, 1, 1, 1), Port: 10131}
	var joined []net.Interface
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if packet.JoinGroup(&iface, group) == nil {
			joined = append(joined, iface)
		}
	}
	if len(joined) == 0 {
		return nil, errors.New("no multicast-capable LAN interface is available")
	}
	queries := [][]byte{[]byte(`{"name":"unitree_dapengche"}`)}
	for _, serial := range serials {
		query, _ := json.Marshal(map[string]string{"name": "unitree_dapengche", "sn": serial})
		queries = append(queries, query)
	}
	// Repeat queries while collecting replies. Listening remains bounded even
	// on a busy network that never lets a per-read inactivity timer expire.
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for attempt := 0; attempt < 3; attempt++ {
			for _, iface := range joined {
				if ctx.Err() != nil {
					return
				}
				if packet.SetMulticastInterface(&iface) != nil {
					continue
				}
				_ = conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
				for _, query := range queries {
					_, _ = conn.WriteToUDP(query, group)
				}
			}
			if attempt < 2 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	}()
	defer func() { _ = conn.Close(); <-senderDone }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	devices := make(map[string]Device)
	buffer := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				break
			}
			return nil, err
		}
		if device, ok := deviceFromDatagram(buffer[:n], from.IP); ok && len(devices) < 256 {
			devices[device.Serial] = device
		}
	}
	list := make([]Device, 0, len(devices))
	for _, device := range devices {
		list = append(list, device)
	}
	return list, nil
}

func deviceFromDatagram(raw []byte, sender net.IP) (Device, bool) {
	var reply struct {
		Serial string `json:"sn"`
		IP     string `json:"ip"`
	}
	if json.Unmarshal(raw, &reply) != nil || !ValidSerial(reply.Serial) {
		return Device{}, false
	}
	address := sender
	if reply.IP != "" {
		address = net.ParseIP(reply.IP)
		// An untrusted LAN reply must not redirect the connection to a host
		// other than its sender. Pairing still authenticates the candidate.
		if address == nil || !address.Equal(sender) {
			return Device{}, false
		}
	}
	if !isRobotIP(address) {
		return Device{}, false
	}
	return Device{Serial: reply.Serial, Name: "Unitree Go2", Address: address.String()}, true
}

func deviceFromMDNS(entry discovery.MDNSService, serials []string) (Device, bool) {
	address := net.ParseIP(entry.IPAddress)
	if !isRobotIP(address) || address.To4() == nil {
		return Device{}, false
	}
	for _, serial := range serials {
		matched := identityHasSerial(entry.InstanceName, serial) || identityHasSerial(entry.Hostname, serial)
		for key, value := range entry.TXTRecords {
			if (strings.EqualFold(key, "sn") || strings.EqualFold(key, "serial")) && value == serial {
				matched = true
			}
		}
		if matched {
			return Device{Serial: serial, Name: "Unitree Go2", Address: address.String()}, true
		}
	}
	return Device{}, false
}

func identityHasSerial(identity, serial string) bool {
	identity = strings.ToLower(identity)
	serial = strings.ToLower(serial)
	for offset := 0; offset < len(identity); {
		index := strings.Index(identity[offset:], serial)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(serial)
		isAlnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' }
		if (start == 0 || !isAlnum(identity[start-1])) && (end == len(identity) || !isAlnum(identity[end])) {
			return true
		}
		offset = end
	}
	return false
}

func isRobotIP(address net.IP) bool {
	return address != nil && !address.IsLoopback() && !address.IsUnspecified() && !address.IsMulticast() && (address.IsGlobalUnicast() || address.IsLinkLocalUnicast())
}
