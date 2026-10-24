// Package vpn implements the split tunnel shared by the app and its extension.
package vpn

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

const DNSAddress = "100.64.0.53"
const TunnelAddress = "100.64.0.54"

var devicePrefix = netip.MustParsePrefix("10.99.0.0/16")

type Route struct {
	Selector string `json:"selector"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Host     string `json:"host"`
}
type Config struct {
	Endpoint string  `json:"endpoint"`
	Token    string  `json:"token"`
	Routes   []Route `json:"routes"`
}

func BuildRoutes(input []Route) ([]Route, error) {
	if len(input) == 0 || len(input) > 1024 {
		return nil, errors.New("select between 1 and 1024 mesh or cloud devices")
	}
	out := make([]Route, 0, len(input))
	used := map[uint16]bool{}
	hosts := map[string]bool{}
	seen := map[string]bool{}
	for _, r := range input {
		var key, zone string
		if strings.HasPrefix(r.Selector, "mesh:") {
			key = strings.TrimPrefix(r.Selector, "mesh:")
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
		if seen[r.Selector] {
			return nil, errors.New("duplicate route")
		}
		seen[r.Selector] = true
		sum := sha256.Sum256([]byte(r.Selector))
		id := binary.BigEndian.Uint16(sum[:2])
		if id == 0 || id == 65535 {
			id = 1
		}
		// Never silently change a previously advertised address on a collision.
		if used[id] {
			return nil, errors.New("device address collision; select fewer devices")
		}
		used[id] = true
		r.Address = fmt.Sprintf("10.99.%d.%d", id>>8, id&255)
		r.Host = "device-" + strings.ToLower(key) + "." + zone
		if hosts[r.Host] {
			return nil, errors.New("DNS name collision across cloud organizations")
		}
		hosts[r.Host] = true
		out = append(out, r)
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
	routes, err := BuildRoutes(c.Routes)
	if err != nil {
		return err
	}
	for i, r := range routes {
		if c.Routes[i].Address != r.Address || c.Routes[i].Host != r.Host {
			return errors.New("invalid route address or DNS name")
		}
	}
	return nil
}
