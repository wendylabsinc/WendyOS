package netmesh

import (
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/waiter"
)

// IP encapsulation has no ports or transport checksum. Registering its protocol
// numbers lets raw endpoints receive reassembled IPv6 payloads through netstack.
type ipProtocol struct {
	stack  *stack.Stack
	number tcpip.TransportProtocolNumber
}

func ipInIPv6(number tcpip.TransportProtocolNumber) stack.TransportProtocolFactory {
	return func(s *stack.Stack) stack.TransportProtocol { return &ipProtocol{stack: s, number: number} }
}
func (p *ipProtocol) Number() tcpip.TransportProtocolNumber { return p.number }
func (p *ipProtocol) NewEndpoint(tcpip.NetworkProtocolNumber, *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	return nil, &tcpip.ErrNotSupported{}
}
func (p *ipProtocol) NewRawEndpoint(network tcpip.NetworkProtocolNumber, queue *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	return raw.NewEndpoint(p.stack, network, p.number, queue)
}
func (p *ipProtocol) MinimumPacketSize() int {
	if p.number == 41 {
		return 40
	}
	return 20
}
func (p *ipProtocol) ParsePorts([]byte) (uint16, uint16, tcpip.Error) { return 0, 0, nil }
func (p *ipProtocol) Parse(packet *stack.PacketBuffer) bool {
	_, ok := packet.TransportHeader().Consume(p.MinimumPacketSize())
	return ok
}
func (*ipProtocol) HandleUnknownDestinationPacket(stack.TransportEndpointID, *stack.PacketBuffer) stack.UnknownDestinationPacketDisposition {
	return stack.UnknownDestinationPacketHandled
}
func (*ipProtocol) SetOption(tcpip.SettableTransportProtocolOption) tcpip.Error {
	return &tcpip.ErrUnknownProtocolOption{}
}
func (*ipProtocol) Option(tcpip.GettableTransportProtocolOption) tcpip.Error {
	return &tcpip.ErrUnknownProtocolOption{}
}
func (*ipProtocol) Close()   {}
func (*ipProtocol) Wait()    {}
func (*ipProtocol) Pause()   {}
func (*ipProtocol) Resume()  {}
func (*ipProtocol) Restore() {}
