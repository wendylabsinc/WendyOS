package netmesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type Device struct {
	Selector  string                   `json:"selector,omitempty"`
	Principal string                   `json:"principal,omitempty"`
	ID        uuid.UUID                `json:"id"`
	Name      string                   `json:"name"`
	Address   netip.Addr               `json:"address"`
	Port      uint16                   `json:"port"`
	Agent     *localmesh.AgentMetadata `json:"agent,omitempty"`
}
type Status struct {
	Identity string            `json:"identity"`
	Devices  []Device          `json:"devices"`
	Carriers map[string]string `json:"carriers"`
	Links    []LinkStatus      `json:"links,omitempty"`
}

// LinkStatus describes the worker's authenticated carriers on its private IPC
// socket. Queue statistics distinguish a slow carrier from an absent route.
type LinkStatus struct {
	Peer      string                  `json:"peer"`
	Transport string                  `json:"transport"`
	SendQueue *localmesh.IPQueueStats `json:"sendQueue,omitempty"`
}
type Client struct {
	node        *localmesh.Node
	network     *Network
	credentials *localmesh.Credentials
	mu          sync.Mutex
	carriers    map[string]string
}

func New(ctx context.Context, dir string, credentials *localmesh.Credentials) (*Client, error) {
	if credentials == nil || !credentials.Device.IsOperator() {
		return nil, fmt.Errorf("desktop mesh requires an operator identity")
	}
	network, err := NewNetwork(credentials.Device)
	if err != nil {
		return nil, err
	}
	node, err := localmesh.NewNodeWithNetwork(ctx, dir, credentials, "", 0, network)
	if err != nil {
		network.Close()
		return nil, err
	}
	return &Client{node: node, network: network, credentials: credentials, carriers: map[string]string{}}, nil
}
func (c *Client) Status() Status {
	sum := sha256.Sum256(c.credentials.Certificate.Certificate[0])
	snapshot := c.node.Snapshot()
	status := Status{Identity: hex.EncodeToString(sum[:]), Devices: devicesFromSnapshot(snapshot, time.Now()), Carriers: map[string]string{}}
	for _, link := range snapshot.Links {
		status.Links = append(status.Links, LinkStatus{Peer: link.Device.UUID().String(), Transport: link.Transport, SendQueue: link.SendQueue})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.carriers {
		status.Carriers[k] = v
	}
	return status
}
func devicesFromSnapshot(snapshot localmesh.NodeSnapshot, now time.Time) []Device {
	var devices []Device
	links := map[uint64]bool{}
	for _, link := range snapshot.Links {
		links[uint64(link.ID)] = true
	}
	for _, m := range snapshot.Devices {
		if m.Owner != nil {
			continue
		}
		if m.AgentPort == 0 || m.Route == nil || m.Withdraw || m.Expires <= now.UnixMilli() || !snapshot.RouteAuthorizations[*m.Route] {
			continue
		}
		device := meshid.NewDevice(snapshot.Self.Tenant(), m.Device)
		for _, r := range snapshot.Routes {
			if !r.Local && !r.Unreachable && links[uint64(r.Link)] && r.Prefix == m.Route.Prefix && r.RouterID == m.Route.RouterID && r.Seqno == m.Route.Seqno && r.Prefix == netip.PrefixFrom(device.MeshAddress(), 128) {
				devices = append(devices, Device{ID: m.Device, Name: m.Name, Address: device.MeshAddress(), Port: m.AgentPort, Agent: m.Agent})
				break
			}
		}
	}
	return devices
}
func Resolve(devices []Device, name string) (Device, error) {
	var found *Device
	for _, d := range devices {
		if d.ID.String() == name || d.Name == name {
			if found != nil {
				return Device{}, fmt.Errorf("ambiguous mesh device %q; use its UUID", name)
			}
			copy := d
			found = &copy
		}
	}
	if found == nil {
		return Device{}, fmt.Errorf("mesh device %q is not reachable", name)
	}
	return *found, nil
}
func (c *Client) Dial(ctx context.Context, id string, port uint16) (net.Conn, error) {
	d, err := Resolve(c.Status().Devices, id)
	if err != nil {
		return nil, err
	}
	return c.network.DialContext(ctx, netip.AddrPortFrom(d.Address, port).String())
}
func (c *Client) carrierStatus(name string) func(localmesh.CarrierStatus) {
	return func(status localmesh.CarrierStatus) {
		text := status.Detail
		if status.Err != nil {
			text = status.Err.Error()
		}
		c.mu.Lock()
		c.carriers[name] = text
		c.mu.Unlock()
	}
}
