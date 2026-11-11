// Package vpn implements the split tunnel shared by the app and its extension.
package vpn

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
)

const DNSAddress = "100.64.0.53"
const TunnelAddress = "100.64.0.54"

var devicePrefix = netip.MustParsePrefix("10.99.0.0/16")

type Route struct {
	Selector  string `json:"selector"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Host      string `json:"host"`
	Principal string `json:"principal,omitempty"`
}
type Config struct {
	BootstrapDirectory string                      `json:"bootstrapDirectory,omitempty"`
	Gateway            string                      `json:"gateway,omitempty"`
	Endpoint           string                      `json:"endpoint"`
	Token              string                      `json:"token"`
	Routes             []Route                     `json:"routes"`
	Internet           *meshsharing.InternetConfig `json:"internet,omitempty"`
}

func BuildRoutes(input []Route) ([]Route, error) {
	if len(input) == 0 || len(input) > 1024 {
		return nil, errors.New("select between 1 and 1024 mesh or cloud devices")
	}
	out := make([]Route, len(input))
	order := make([]int, len(input))
	for i := range input {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return input[order[i]].Selector < input[order[j]].Selector })
	used := map[uint16]bool{}
	hosts := map[string]bool{}
	seen := map[string]bool{}
	for _, index := range order {
		r := input[index]
		var key, zone string
		if strings.HasPrefix(r.Selector, "mesh:") {
			key = strings.TrimPrefix(r.Selector, "mesh:")
			if strings.HasPrefix(r.Selector, "mesh://") {
				u, err := url.Parse(r.Selector)
				if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.ForceQuery {
					return nil, errors.New("invalid mesh selector")
				}
				tenant, err := meshid.ParseTenant(u.Host)
				if err != nil {
					return nil, err
				}
				device, err := meshid.ParseDevice(tenant, strings.TrimPrefix(u.Path, "/"))
				if err != nil {
					return nil, err
				}
				if r.Selector != "mesh://"+tenant.String()+"/"+device.UUID().String() {
					return nil, errors.New("invalid mesh selector")
				}
				key = device.UUID().String()
			}
			zone = "mesh.wendy.dev"
		} else if strings.HasPrefix(r.Selector, "cloud://") {
			u, err := url.Parse(r.Selector)
			if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, errors.New("invalid cloud selector")
			}
			parts := strings.Split(u.Path, "/")
			if len(parts) != 5 || (parts[1] != "tenant" && parts[1] != "org") || parts[3] != "asset" || parts[2] == "" {
				return nil, errors.New("invalid cloud selector")
			}
			key = parts[4]
			zone = "cloud.wendy.dev"
		} else {
			return nil, errors.New("only mesh and cloud devices can be routed")
		}
		if len(key) == 0 || len(key) > 48 {
			return nil, errors.New("invalid device identifier")
		}
		for _, c := range key {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return nil, errors.New("invalid device identifier")
			}
		}
		// A route principal is authenticated inventory metadata, never an
		// asset-ID guess. Mesh selectors already carry the exact principal.
		if strings.HasPrefix(r.Selector, "mesh://") {
			parsed, _, err := routeDevice(r)
			if err != nil {
				return nil, err
			}
			r.Principal = parsed.Principal()
		} else if r.Principal != "" {
			if _, _, err := routeDevice(r); err != nil {
				return nil, err
			}
		}
		if seen[r.Selector] {
			return nil, errors.New("duplicate route")
		}
		seen[r.Selector] = true
		sum := sha256.Sum256([]byte(r.Selector))
		id := binary.BigEndian.Uint16(sum[:2])
		if id == 0 || id == 65535 {
			id = 1
		}
		// Allocate colliding hashes in selector order so discovery ordering
		// cannot change their addresses, and every device can be included.
		for used[id] {
			id++
			if id == 65535 || id == 0 {
				id = 1
			}
		}
		used[id] = true
		// Only canonical device UUIDs enter the shared private namespace. Legacy
		// numeric or opaque identifiers keep their original compatibility name.
		if _, err := meshid.ParseDeviceUUID(strings.ToLower(key)); err == nil && strings.HasPrefix(r.Selector, "mesh://") {
			zone = meshname.Suffix
		}
		r.Address = fmt.Sprintf("10.99.%d.%d", id>>8, id&255)
		r.Host = "device-" + strings.ToLower(key) + "." + zone
		if hosts[r.Host] {
			return nil, errors.New("DNS name collision across cloud organizations")
		}
		hosts[r.Host] = true
		out[index] = r
	}
	return out, nil
}
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil || host != "127.0.0.1" || port == "0" {
		return errors.New("relay must use IPv4 loopback")
	}
	if len(c.Token) != 64 {
		return errors.New("missing relay credential")
	}
	if c.Internet != nil && !c.Internet.Valid() {
		return errors.New("invalid internet lease")
	}
	if len(c.Routes) == 0 {
		return nil
	}
	routes, err := BuildRoutes(c.Routes)
	if err != nil {
		return err
	}
	for i, r := range routes {
		if c.Routes[i].Address != r.Address || !compatibleRouteHost(c.Routes[i], r.Host) {
			return errors.New("invalid route address or DNS name")
		}
	}
	return nil
}

// Persisted VPN configurations may still carry the previous generated mesh
// hostname. Accept only aliases of the same proven device identity; route
// addresses and selectors are still independently re-derived above.
func compatibleRouteHost(route Route, canonical string) bool {
	if strings.EqualFold(route.Host, canonical) {
		return true
	}
	if !strings.HasPrefix(route.Selector, "mesh:") {
		return false
	}
	label, _, ok := deviceDNSLabel(strings.ToLower(dns.Fqdn(route.Host)))
	want, _, valid := deviceDNSLabel(strings.ToLower(dns.Fqdn(canonical)))
	if !ok || !valid || label != want || !strings.HasPrefix(label, "device-") {
		return false
	}
	_, err := meshid.ParseDeviceUUID(strings.TrimPrefix(label, "device-"))
	return err == nil
}

// routeDevice checks a route's device identity without conflating a Cloud
// database row ID with its enrolled certificate identity.
func routeDevice(route Route) (meshid.Device, bool, error) {
	u, err := url.Parse(route.Selector)
	if err != nil {
		return meshid.Device{}, false, err
	}
	var expected meshid.Device
	var tenant string
	if u.Scheme == "mesh" && u.Host != "" {
		t, e := meshid.ParseTenant(u.Host)
		if e != nil {
			return expected, false, e
		}
		expected, e = meshid.ParseDevice(t, strings.TrimPrefix(u.Path, "/"))
		if e != nil {
			return expected, false, e
		}
		tenant = t.String()
	} else if u.Scheme == "cloud" {
		parts := strings.Split(u.Path, "/")
		if len(parts) == 5 && parts[1] == "tenant" {
			tenant = parts[2]
		}
	}
	if route.Principal != "" {
		peer, e := meshid.DeviceFromPrincipal(route.Principal)
		if e != nil {
			return expected, false, e
		}
		if tenant == "" || peer.Tenant().String() != tenant || expected.IsValid() && peer != expected {
			return expected, false, errors.New("route principal does not match its selected tenant/device")
		}
		return peer, true, nil
	}
	return expected, expected.IsValid(), nil
}
