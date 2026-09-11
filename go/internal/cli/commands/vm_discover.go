package commands

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// vmDeviceIDPrefix marks a discovery row as a local VM rather than a device on
// the network. Dotless, so resolveTargetInner still treats it as a provider-ish
// key rather than an address.
const vmDeviceIDPrefix = "vm:"

// vmProbeBudget bounds the whole sweep, matching usbDirectProbeBudget's role
// for the USB probe.
const vmProbeBudget = 8 * time.Second

// vmStatusesFn reports what the local VM store holds. A seam so tests can
// describe a set of VMs directly, rather than having to hold a real run lock
// to make one look running.
var vmStatusesFn = func() ([]vm.Status, error) {
	store, err := vm.NewStore()
	if err != nil {
		return nil, err
	}
	return store.Statuses()
}

// vmRecordHostnameFn remembers the hostname a VM's guest reported. A seam so
// tests never write to the developer's real VM store.
var vmRecordHostnameFn = func(name, hostname string) error {
	store, err := vm.NewStore()
	if err != nil {
		return err
	}
	return store.RecordHostname(name, hostname)
}

// vmHostAddr maps a guest agent port onto the host side of a VM's forward.
// The forward maps host port onto the guest's plaintext port and port+1 onto
// its mTLS port, so a guest port shifts by the same delta to reach the host.
func vmHostAddr(hostPort, guestPort int) string {
	return fmt.Sprintf("127.0.0.1:%d", hostPort+guestPort-defaultAgentPort)
}

// probeLocalVMDevices returns a row for every running user-mode VM whose agent
// answers on its forwarded loopback port, and records each guest's hostname so
// its stray mDNS announcement is recognised from then on.
//
// Shared-mode VMs are skipped: they are on a real segment where mDNS finds
// them. Those reachable sightings stay on Local; only leaked user-network
// sightings are filtered. User-mode VMs are reachable only through
// the forward, so this probe is the only way to list them with an address.
func probeLocalVMDevices(ctx context.Context) []models.LANDevice {
	statuses, err := vmStatusesFn()
	if err != nil {
		return nil
	}

	type candidate struct {
		name string
		port int
	}
	var candidates []candidate
	for _, st := range statuses {
		if st.Running && st.State.NetMode == vm.NetUser && st.State.AgentPort != 0 {
			candidates = append(candidates, candidate{st.Name, st.State.AgentPort})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Bounded like the USB probe: a QEMU forward accepts on the host before it
	// checks the guest, so a booting VM passes the connect gate and reaches the
	// full auto-TLS ladder. Without a budget discovery would block on it.
	pctx, cancel := context.WithTimeout(ctx, vmProbeBudget)
	defer cancel()

	results := make([]*models.LANDevice, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c candidate) {
			defer wg.Done()
			hostAddrFor := func(guestPort int) string { return vmHostAddr(c.port, guestPort) }
			if !anyAgentPortAnswers(pctx, hostAddrFor) {
				return
			}
			conn, resp, err := probeSimulatorAgent(pctx, c.name, hostAddrFor(defaultAgentPort))
			if err != nil || resp == nil {
				return
			}
			defer conn.Close()
			isMTLS := conn.IsMTLS
			// Best-effort: a store that cannot be written costs the next session
			// one re-learn, nothing more.
			_ = vmRecordHostnameFn(c.name, resp.GetHostname())
			advertisedPort := c.port
			if isMTLS {
				advertisedPort += agentMTLSPortOffset
			}
			results[i] = &models.LANDevice{
				ID:          vmDeviceIDPrefix + c.name,
				DisplayName: c.name,
				// Hostname stays empty on purpose. Every VM booted from the
				// same image reports the same guest hostname, and the dedup key
				// prefers the hostname -- two VMs would collapse into one row.
				IPAddress: "127.0.0.1",
				// A provisioned agent advertises its mTLS port; lanAgentAddresses
				// subtracts the offset back off to recover what to dial.
				Port:            advertisedPort,
				IsMTLS:          isMTLS,
				InterfaceType:   string(models.InterfaceLAN),
				IsWendyDevice:   true,
				AgentVersion:    resp.GetVersion(),
				OS:              resp.GetOs(),
				OSVersion:       resp.GetOsVersion(),
				CPUArchitecture: resp.GetCpuArchitecture(),
				DeviceType:      resp.GetDeviceType(),
			}
		}(i, c)
	}
	wg.Wait()

	var out []models.LANDevice
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// discoverLocalTargets runs ordinary discovery plus the sources that only this
// machine can see: USB-direct links and local VMs.
// The launcher forwards a span of host ports starting at its own default, and
// that span has to cover the port provisioning moves the agent to -- otherwise
// a provisioned VM silently goes unreachable. The constants live in two
// packages, so this is what keeps them honest: a mismatch fails to compile.
var (
	_ = [1]struct{}{}[defaultAgentPort-vm.DefaultAgentPort]
	_ = [1]struct{}{}[vm.AgentPortSpan-(agentMTLSPortOffset+1)]
)

func discoverLocalTargets(ctx context.Context, opts discovery.DiscoveryOptions) (*models.DevicesCollection, error) {
	var vms []models.LANDevice
	var wg sync.WaitGroup
	// Same filter as the USB-direct probe: both synthesise LAN-type rows, so
	// both run exactly when LAN discovery would.
	if shouldProbeUSBDirect(opts) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vms = probeLocalVMDevices(ctx)
		}()
	}
	collection, err := discoverWithUSBDirect(ctx, opts)
	wg.Wait()
	if collection == nil {
		collection = &models.DevicesCollection{}
	}
	// Merged in even when LAN discovery failed, so the rows are there for a
	// caller that renders a partial result. Both of today's callers surface
	// the error instead, which is why the error is still returned.
	attachSimulators(collection, vms)
	return collection, err
}

// attachSimulators lists the VM rows apart from the LAN devices. A VM is
// reached like a LAN device but is not one, and a JSON reader asking for
// devices on the network must not have to filter it out.
func attachSimulators(collection *models.DevicesCollection, vms []models.LANDevice) {
	collection.Simulators = append(collection.Simulators, vms...)
}

// isSimulatorBoard reports whether a device type names one of the Builder's
// VM boards (vmDeviceKey and its x86 sibling). A positive signal: no physical
// board id starts this way, so a real device can never be filed as a VM.
func isSimulatorBoard(deviceType string) bool {
	return strings.HasPrefix(deviceType, "vm-")
}

// simulatorHostnameRetry is how often the filter re-asks a running VM that has
// not answered yet, and simulatorHostnameAskBudget bounds one ask: QEMU's
// forward accepts before the guest listens, so a booting VM passes the connect
// gate and then hangs the TLS ladder without a bound.
var (
	simulatorHostnameRetry     = 2 * time.Second
	simulatorHostnameAskBudget = 3 * time.Second
)

// simulatorFilter suppresses leaked user-network announcements, not all VMs.
// At the default SLIRP address, it uses either of two signals:
//   - the device type is a VM board, whether from the devicetype TXT record
//     (classified on the sighting itself) or from an agent probe;
//   - with no board advertised, the hostname is one a local VM reported. This
//     catches a user-mode VM's leaked announcement, which advertises an
//     address (10.0.2.15) no probe can reach, on images whose agent predates
//     the TXT record.
type simulatorFilter struct {
	mu        sync.Mutex
	hostnames map[string]bool // in LANDevice.HostKey form
	changed   chan struct{}
}

// newSimulatorFilter loads every hostname the VM store has on record and, for
// each running user-mode VM without one, starts asking its agent. ctx bounds
// those learners; tie it to the discovery session.
func newSimulatorFilter(ctx context.Context) *simulatorFilter {
	f := &simulatorFilter{hostnames: make(map[string]bool), changed: make(chan struct{}, 1)}
	statuses, err := vmStatusesFn()
	if err != nil {
		return f // an unreadable store leaves the board signal, which needs no store
	}
	// Finish seeding before publishing the map to any learner goroutine.
	var learners []vm.Status
	for _, st := range statuses {
		if st.Meta.Hostname != "" {
			// Stopped VMs count too: a stale sighting or cache row of one has
			// to go the same way.
			f.hostnames[vmHostKey(st.Meta.Hostname)] = true
			continue
		}
		if st.Running && st.State.NetMode == vm.NetUser && st.State.AgentPort != 0 {
			learners = append(learners, st)
		}
	}
	for _, st := range learners {
		go f.learn(ctx, st.Name, st.State.AgentPort)
	}
	return f
}

// vmHostKey normalises a hostname the way LANDevice.HostKey does, so a
// recorded name and a sighting's compare equal regardless of case or a
// trailing .local.
func vmHostKey(hostname string) string {
	return models.LANDevice{Hostname: hostname}.HostKey()
}

func (f *simulatorFilter) Exclude(dev models.LANDevice) bool {
	if isSimulatorBoard(dev.DeviceType) {
		// A board id identifies a VM, not its owner or networking mode.
		// Preserve directly reachable shared/network VMs. The launcher uses
		// QEMU's default user-network guest address for leaked announcements.
		return dev.IPAddress == "10.0.2.15"
	}
	// A hostname is a reusable label, not a durable device identity. Never
	// hide a physical board or a normally addressed device just because a VM
	// (including a stopped one) once used its name. Legacy images without a
	// board TXT record get the fallback only at the leaked SLIRP address.
	if dev.DeviceType != "" || dev.IPAddress != "10.0.2.15" {
		return false
	}
	host := dev.HostKey()
	if host == "" {
		return false // a display name alone is no identity
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostnames[host]
}

func (f *simulatorFilter) Changed() <-chan struct{} { return f.changed }

// learn asks a running VM's agent for its hostname over the loopback forward,
// retrying while the guest boots, then records it and signals the session to
// re-check what it has listed. Bounded by the boot budget: a VM that never
// answers is one that never announced itself either.
func (f *simulatorFilter) learn(ctx context.Context, name string, port int) {
	deadline := time.Now().Add(simulatorBootTimeout())
	for {
		if hostname := askVMHostname(ctx, name, port); hostname != "" {
			// Best-effort: a store that cannot be written costs the next
			// session one re-learn, nothing more.
			_ = vmRecordHostnameFn(name, hostname)
			f.mu.Lock()
			f.hostnames[vmHostKey(hostname)] = true
			f.mu.Unlock()
			// Non-blocking: one pending signal covers any number of learners,
			// since the session re-checks everything on each.
			select {
			case f.changed <- struct{}{}:
			default:
			}
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(simulatorHostnameRetry):
		}
	}
}

// askVMHostname makes one bounded attempt to read the guest's hostname
// through the forward at port; "" when the agent is not answering yet.
func askVMHostname(ctx context.Context, name string, port int) string {
	actx, cancel := context.WithTimeout(ctx, simulatorHostnameAskBudget)
	defer cancel()
	hostAddrFor := func(guestPort int) string { return vmHostAddr(port, guestPort) }
	if !anyAgentPortAnswers(actx, hostAddrFor) {
		return ""
	}
	conn, resp, err := probeSimulatorAgent(actx, name, hostAddrFor(defaultAgentPort))
	if err != nil || resp == nil {
		return ""
	}
	defer conn.Close()
	return resp.GetHostname()
}
