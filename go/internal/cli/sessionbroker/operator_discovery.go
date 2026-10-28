package sessionbroker

import (
	"context"
	"errors"
	"net"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

const operatorLANService = "_wendy-net._tcp"
const operatorLANCandidates = 64

// Native mDNS is installed by the host-discovery layer. A nil hook disables
// native browsing while preserving explicit peers and injected fixture sources.
var browseOperatorLAN func(context.Context, string) (<-chan discovery.MDNSService, error)

// operatorLANCandidate is an untrusted connection hint, never an identity proof.
// Open still proves the exact advertised device and selected operator tenant.
func operatorLANCandidate(service discovery.MDNSService, operator meshoperator.Operator) (OperatorSpec, bool) {
	txt := service.TXTRecords
	if txt["platform"] != "WendyOS" || txt["protocol"] != "tls-v1" || txt["version"] != "1" || txt["role"] != "router" {
		return OperatorSpec{}, false
	}
	peer, err := meshid.DeviceFromPrincipal(txt["principal"])
	if err != nil || peer.Tenant() != operator.Tenant() || service.Port < 1 || service.Port > 65535 {
		return OperatorSpec{}, false
	}
	ip := net.ParseIP(service.IPAddress)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return OperatorSpec{}, false
	}
	host := ip.String()
	if ip.IsLinkLocalUnicast() && ip.To4() == nil {
		if service.InterfaceName == "" {
			return OperatorSpec{}, false
		}
		host += "%" + service.InterfaceName
	}
	return OperatorSpec{Version: 1, Principal: operator.Principal(), RouterUUID: peer.UUID().String(), RouterAddress: net.JoinHostPort(host, strconv.Itoa(service.Port))}, true
}

// discoverOperatorLAN performs only a bounded network browse. In particular it
// never opens USB/serial devices as general physical-device discovery can do.
func discoverOperatorLAN(ctx context.Context, operator meshoperator.Operator) ([]OperatorSpec, error) {
	if browseOperatorLAN == nil {
		return nil, ctx.Err()
	}
	browse, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	services, err := browseOperatorLAN(browse, operatorLANService)
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]OperatorSpec)
	var settled *time.Timer
	var settle <-chan time.Time
	defer func() {
		if settled != nil {
			settled.Stop()
		}
	}()
	for {
		select {
		case service, ok := <-services:
			if !ok {
				return sortedOperatorCandidates(candidates), nil
			}
			candidate, ok := operatorLANCandidate(service, operator)
			if !ok {
				continue
			}
			key := candidate.RouterUUID + "@" + candidate.RouterAddress
			if _, seen := candidates[key]; seen || len(candidates) == operatorLANCandidates {
				continue
			}
			candidates[key] = candidate
			if settled == nil {
				settled = time.NewTimer(250 * time.Millisecond)
				settle = settled.C
			}
		case <-settle:
			return sortedOperatorCandidates(candidates), nil
		case <-browse.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return sortedOperatorCandidates(candidates), nil
		}
	}
}

func sortedOperatorCandidates(candidates map[string]OperatorSpec) []OperatorSpec {
	out := make([]OperatorSpec, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool {
		// Stable ordering avoids changing the selected peer on duplicate answers.
		if out[i].RouterUUID != out[j].RouterUUID {
			return out[i].RouterUUID < out[j].RouterUUID
		}
		return out[i].RouterAddress < out[j].RouterAddress
	})
	return out
}

// PrepareOperatorAutomatic finds an ordinary WendyOS first hop on the LAN.
// Explicit --address/--router remains available for a deliberate attachment.
func PrepareOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo) (*grpcclient.AgentConnection, error) {
	return prepareOperatorAutomatic(ctx, certificate, false)
}

// EnableOperatorAutomatic retains an explicitly enabled router independently of
// ordinary command leases. Passive cold discovery alone may still expire idle.
func EnableOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo) (*grpcclient.AgentConnection, error) {
	return prepareOperatorAutomatic(ctx, certificate, true)
}
func prepareOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo, enabled bool) (*grpcclient.AgentConnection, error) {
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		return nil, err
	}
	// Own discovery/recipe creation across independent processes, not merely
	// startup after selecting a peer. Concurrent cold callers can see different
	// multicast answers; the first recipe wins and the others reuse that owner.
	if !configDirOwnershipSafe() {
		return nil, errors.New("operator profile is not owned by the current user")
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	brokerDirectory, err := brokerDir()
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(brokerDirectory); err != nil {
		return nil, err
	}
	socket, _ := paths(brokerDirectory, operatorKey(operator.Principal()), certs.WendyIdentity{})
	var lock *os.File
	for lock == nil {
		file, acquired, err := acquireIdentityLock(socket + ".discovery")
		if err != nil {
			return nil, err
		}
		if acquired {
			lock = file
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { releaseLock(lock); _ = lock.Close() }()
	if recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal())); err == nil {
		if enabled && !recipe.Enabled {
			recipe.Enabled = true
			return prepareOperatorRecipe(ctx, certificate, recipe)
		}
		spec, err := operatorSpec(recipe, certificate)
		if err != nil {
			return nil, err
		}
		return connectOrStartOperator(ctx, spec, &certificate)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// The detached broker creates its own signed origin before discovering any
	// first hop. Ordinary bounded LAN/BLE candidates then establish verified TLS.
	return prepareOperatorRecipe(ctx, certificate, OperatorSpec{Version: 1, Principal: operator.Principal(), Automatic: true, Enabled: enabled})
}
