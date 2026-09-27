//go:build darwin

package discovery

/*
#include <dns_sd.h>
#include <stdlib.h>
#include "dnssd_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mDNS browse and resolve against mDNSResponder through <dns_sd.h>, which is
// what the dns-sd command line tool itself wraps. Talking to the daemon
// in-process keeps the interface coverage that motivated shelling out — the
// system resolver sees interfaces raw multicast misses, notably USB host-mode —
// while removing the helper processes. A spawned dns-sd outlives a parent that
// dies without unwinding (macOS has no PR_SET_PDEATHSIG), and those orphans
// accumulate under sustained polling until the per-user process limit is hit
// (WDY-1831). A daemon connection cannot outlive the process holding it.
//
// The API also returns structured records, so instance names and TXT values no
// longer have to be recovered by parsing dns-sd's human-readable output.

// dnssdPollInterval bounds how long a session waits on its socket before
// rechecking ctx. It caps cancellation latency; results themselves arrive as
// soon as the socket is readable.
const dnssdPollInterval = 100 * time.Millisecond

// dnssdResolveTimeout bounds a single instance resolve on the streaming LAN
// backend (mdnsStreamBackend). It is a var, not a const, so tests can shrink
// it.
var dnssdResolveTimeout = 1 * time.Second

// dnssdSession carries the callbacks for one DNSServiceRef. It is reached from
// C by an integer handle: cgo forbids passing Go pointers into C and holding
// them there.
type dnssdSession struct {
	onBrowse  func(flags uint32, ifIndex uint32, name, domain string)
	onResolve func(host string, port int, txt []byte)
	onAddress func(flags uint32, ifIndex uint32, address string)

	// err and stop are written by the reply callback and read by the poll loop
	// that invoked DNSServiceProcessResult. Callbacks run synchronously on that
	// same goroutine, so no lock is needed.
	err  error
	stop bool
}

var (
	dnssdMu       sync.Mutex
	dnssdSessions = make(map[uintptr]*dnssdSession)
	dnssdNextID   uintptr
)

func registerSession(s *dnssdSession) uintptr {
	dnssdMu.Lock()
	defer dnssdMu.Unlock()
	dnssdNextID++
	id := dnssdNextID
	dnssdSessions[id] = s
	return id
}

func unregisterSession(id uintptr) {
	dnssdMu.Lock()
	defer dnssdMu.Unlock()
	delete(dnssdSessions, id)
}

func lookupSession(id uintptr) *dnssdSession {
	dnssdMu.Lock()
	defer dnssdMu.Unlock()
	return dnssdSessions[id]
}

// dnssdError converts a DNSServiceErrorType into an error, or nil on success.
func dnssdError(code C.DNSServiceErrorType) error {
	if code == C.kDNSServiceErr_NoError {
		return nil
	}
	return fmt.Errorf("dns-sd error %d", int(code))
}

//export wendyDNSSDBrowseReply
func wendyDNSSDBrowseReply(handle C.uintptr_t, flags C.uint32_t, ifIndex C.uint32_t, errCode C.int32_t, name *C.char, domain *C.char) {
	s := lookupSession(uintptr(handle))
	if s == nil {
		return
	}
	if err := dnssdError(C.DNSServiceErrorType(errCode)); err != nil {
		s.err = err
		return
	}
	// Removals share this callback and are distinguished only by the Add flag.
	if uint32(flags)&uint32(C.kDNSServiceFlagsAdd) == 0 {
		return
	}
	s.onBrowse(uint32(flags), uint32(ifIndex), C.GoString(name), C.GoString(domain))
}

//export wendyDNSSDResolveReply
func wendyDNSSDResolveReply(handle C.uintptr_t, errCode C.int32_t, host *C.char, port C.uint16_t, txt unsafe.Pointer, txtLen C.uint16_t) {
	s := lookupSession(uintptr(handle))
	if s == nil {
		return
	}
	if err := dnssdError(C.DNSServiceErrorType(errCode)); err != nil {
		s.err = err
		s.stop = true
		return
	}
	s.onResolve(C.GoString(host), int(port), C.GoBytes(txt, C.int(txtLen)))
	// One reply is enough; the old code killed dns-sd at the first resolved line.
	s.stop = true
}

//export wendyDNSSDAddrReply
func wendyDNSSDAddrReply(handle C.uintptr_t, flags C.uint32_t, ifIndex C.uint32_t, errCode C.int32_t, address *C.char) {
	s := lookupSession(uintptr(handle))
	if s == nil {
		return
	}
	if err := dnssdError(C.DNSServiceErrorType(errCode)); err != nil {
		s.err = err
		s.stop = true
		return
	}
	s.onAddress(uint32(flags), uint32(ifIndex), C.GoString(address))
	// mDNSResponder marks all but the final answer in the current batch with
	// MoreComing. Waiting for that final callback lets the caller prefer IPv4
	// without turning a single address lookup into a long-lived browse.
	if uint32(flags)&uint32(C.kDNSServiceFlagsMoreComing) == 0 {
		s.stop = true
	}
}

// runDNSSDSession starts an operation and pumps its socket until ctx ends, the
// callback sets stop, or an error occurs. Deallocating the ref closes the
// daemon connection, so nothing survives this function.
func runDNSSDSession(ctx context.Context, s *dnssdSession, start func(*C.DNSServiceRef, C.uintptr_t) C.DNSServiceErrorType) error {
	handle := registerSession(s)
	defer unregisterSession(handle)

	var ref C.DNSServiceRef
	if err := dnssdError(start(&ref, C.uintptr_t(handle))); err != nil {
		return err
	}
	defer C.DNSServiceRefDeallocate(ref)

	fd := C.DNSServiceRefSockFD(ref)
	if fd < 0 {
		return errors.New("dns-sd: no socket for service ref")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pollFds, int(dnssdPollInterval.Milliseconds()))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("polling dns-sd socket: %w", err)
		}
		if n == 0 {
			continue
		}

		// Invokes the reply callback synchronously on this goroutine.
		if err := dnssdError(C.DNSServiceProcessResult(ref)); err != nil {
			return err
		}
		if s.err != nil {
			return s.err
		}
		if s.stop {
			return nil
		}
	}
}

// dnssdBrowseStream browses serviceType and calls onResult for each instance
// added, until ctx ends. It does not decide when enough results have arrived —
// that is the caller's policy.
func dnssdBrowseStream(ctx context.Context, serviceType string, onResult func(browseResult)) error {
	cServiceType := C.CString(serviceType)
	defer C.free(unsafe.Pointer(cServiceType))

	session := &dnssdSession{
		onBrowse: func(_ uint32, ifIndex uint32, name, domain string) {
			interfaceName := ""
			// Silently leaves interfaceName empty when the interface disappears
			// between browse and resolve; USB detection is skipped for that
			// device rather than failing the browse.
			if iface, err := net.InterfaceByIndex(int(ifIndex)); err == nil {
				interfaceName = iface.Name
			}
			onResult(browseResult{
				instanceName:   name,
				domain:         domain,
				interfaceName:  interfaceName,
				interfaceIndex: ifIndex,
			})
		},
	}

	err := runDNSSDSession(ctx, session, func(ref *C.DNSServiceRef, handle C.uintptr_t) C.DNSServiceErrorType {
		return C.wendy_dnssd_browse(ref, cServiceType, handle)
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// dnssdRegister advertises a service until the returned stop func is called.
// It exists for tests: it lets the browse and resolve paths run against a known
// service without a device on the network. cgo cannot be used from _test.go
// files, so the helper has to live here.
func dnssdRegister(instance, serviceType string, port uint16, txt map[string]string) (func(), error) {
	cInstance := C.CString(instance)
	defer C.free(unsafe.Pointer(cInstance))
	cServiceType := C.CString(serviceType)
	defer C.free(unsafe.Pointer(cServiceType))

	var txtWire []byte
	for k, v := range txt {
		entry := k + "=" + v
		txtWire = append(txtWire, byte(len(entry)))
		txtWire = append(txtWire, entry...)
	}
	var txtPtr unsafe.Pointer
	if len(txtWire) > 0 {
		txtPtr = C.CBytes(txtWire)
		defer C.free(txtPtr)
	}

	var ref C.DNSServiceRef
	// DNSServiceRegister takes the port in network byte order, and accepts a
	// nil callback when the caller does not need the confirmation reply.
	netPort := C.uint16_t(port<<8 | port>>8)
	err := dnssdError(C.DNSServiceRegister(&ref, 0, C.kDNSServiceInterfaceIndexAny,
		cInstance, cServiceType, nil, nil, netPort,
		C.uint16_t(len(txtWire)), txtPtr, nil, nil))
	if err != nil {
		return nil, err
	}
	return func() { C.DNSServiceRefDeallocate(ref) }, nil
}

// dnssdResolveInstance resolves one browse result to its hostname, port and TXT
// records.
func dnssdResolveInstance(ctx context.Context, inst browseResult, serviceType string) (string, int, map[string]string, error) {
	cName := C.CString(inst.instanceName)
	defer C.free(unsafe.Pointer(cName))
	cServiceType := C.CString(serviceType)
	defer C.free(unsafe.Pointer(cServiceType))
	cDomain := C.CString(inst.domain)
	defer C.free(unsafe.Pointer(cDomain))

	var (
		hostname   string
		port       int
		txtRecords map[string]string
	)
	session := &dnssdSession{
		onResolve: func(host string, p int, txt []byte) {
			hostname = strings.TrimSuffix(host, ".")
			port = p
			txtRecords = parseTXTRecord(txt)
		},
	}

	err := runDNSSDSession(ctx, session, func(ref *C.DNSServiceRef, handle C.uintptr_t) C.DNSServiceErrorType {
		return C.wendy_dnssd_resolve(ref, cName, cServiceType, cDomain, C.uint32_t(inst.interfaceIndex), handle)
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return "", 0, nil, err
	}
	if hostname == "" {
		return "", 0, nil, fmt.Errorf("could not resolve instance %q", inst.instanceName)
	}
	if txtRecords == nil {
		txtRecords = make(map[string]string)
	}
	return hostname, port, txtRecords, nil
}

// dnssdResolveAddresses asks mDNSResponder for addresses on the exact
// interface that produced the browse answer. A global LookupHost here can
// return a Wi-Fi A record for an instance seen over USB Ethernet, producing an
// internally inconsistent "en7 + Wi-Fi IP" device and routing the deployment
// over the wrong link.
func dnssdResolveAddresses(ctx context.Context, hostname string, inst browseResult) ([]string, error) {
	cHostname := C.CString(hostname)
	defer C.free(unsafe.Pointer(cHostname))

	var addresses []string
	session := &dnssdSession{
		onAddress: func(_ uint32, ifIndex uint32, address string) {
			if address == "" {
				return
			}
			ip := net.ParseIP(address)
			if ip != nil && ip.IsLinkLocalUnicast() && strings.Contains(address, ":") {
				zone := inst.interfaceName
				if zone == "" {
					if iface, err := net.InterfaceByIndex(int(ifIndex)); err == nil {
						zone = iface.Name
					}
				}
				if zone != "" {
					address += "%" + LinkLocalDialZone(zone)
				}
			}
			addresses = append(addresses, address)
		},
	}
	err := runDNSSDSession(ctx, session, func(ref *C.DNSServiceRef, handle C.uintptr_t) C.DNSServiceErrorType {
		return C.wendy_dnssd_getaddrinfo(ref, cHostname, C.uint32_t(inst.interfaceIndex), handle)
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("could not resolve address for %q on interface %q", hostname, inst.interfaceName)
	}
	return addresses, nil
}
