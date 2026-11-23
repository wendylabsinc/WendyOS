package backend

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A VPN session owns at most one probe connection per selected device. Only
// transports are reused: every echo must receive a new response from the agent.
type vpnProbes struct {
	ctx     context.Context
	cancel  context.CancelFunc
	connect func(context.Context, string) (*grpcclient.AgentConnection, error)
	devices map[string]*vpnProbeDevice // immutable after construction
	tunnels commands.TunnelSession
}

type vpnProbeDevice struct {
	gate   chan struct{} // serializes connection setup, RPCs and cleanup
	agent  *grpcclient.AgentConnection
	cancel context.CancelFunc
}

func newVPNProbes(routes []vpn.Route, connect func(context.Context, string) (*grpcclient.AgentConnection, error)) *vpnProbes {
	ctx, cancel := context.WithCancel(context.Background())
	p := &vpnProbes{ctx: ctx, cancel: cancel, connect: connect, devices: make(map[string]*vpnProbeDevice)}
	p.tunnels.SetActive(true)
	for _, route := range routes {
		p.devices[route.Selector] = &vpnProbeDevice{gate: make(chan struct{}, 1)}
	}
	return p
}

func (p *vpnProbes) tunnelContext(ctx context.Context, selector string) context.Context {
	if strings.HasPrefix(selector, "cloud://") {
		return p.tunnels.Context(ctx)
	}
	return ctx
}

func (p *vpnProbes) Dial(ctx context.Context, selector string, port uint16) (net.Conn, error) {
	return desktopDial(p.tunnelContext(ctx, selector), selector, port)
}

func (p *vpnProbes) Probe(ctx context.Context, selector string) error {
	d, ok := p.devices[selector]
	if !ok {
		return errors.New("device is not selected for VPN probes")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	select {
	case d.gate <- struct{}{}:
		defer func() { <-d.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.agent == nil {
		// The transport must outlive this echo's deadline. Until the first
		// RPC completes, its cancellation still interrupts setup/lazy dialing.
		life, cancelLife := context.WithCancel(p.ctx)
		stopSetup := context.AfterFunc(ctx, cancelLife)
		defer stopSetup()
		agent, err := p.connect(p.tunnelContext(life, selector), selector)
		if err != nil {
			cancelLife()
			return err
		}
		d.agent, d.cancel = agent, cancelLife
	}
	_, err := d.agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		d.close()
	}
	return err
}

func (d *vpnProbeDevice) close() {
	if d.agent != nil {
		d.cancel()
		d.agent.Close()
		d.agent, d.cancel = nil, nil
	}
}

func (p *vpnProbes) Close() {
	p.cancel()
	p.tunnels.SetActive(false)
	for _, d := range p.devices {
		d.gate <- struct{}{}
		d.close()
		<-d.gate
	}
}
