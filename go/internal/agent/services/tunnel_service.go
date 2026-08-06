package services

import (
	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// TunnelService exposes authenticated LAN datagram sessions. The TCP Tunnel
// method in the shared wire contract is intentionally left unimplemented.
type TunnelService struct {
	agentpbv2.UnimplementedWendyTunnelServiceServer
	logger *zap.Logger
}

func NewTunnelService(logger *zap.Logger) *TunnelService {
	return &TunnelService{logger: logger}
}

// DatagramTunnel serves one multiplexed UDP + ICMP-echo session for a LAN
// client. The existing datagram relay keeps every UDP destination on agent
// loopback and treats echo frames as application-level replies; it does not
// open a raw ICMP socket.
func (s *TunnelService) DatagramTunnel(stream agentpbv2.WendyTunnelService_DatagramTunnelServer) error {
	newDatagramRelay(s.logger, deviceFrameStream{stream: stream}, datagramFlowIdleTimeout).run(stream.Context())
	return nil
}

// deviceFrameStream adapts the LAN service's wire messages to the Cloud frame
// types used internally by the existing datagram relay. It keeps the current
// Cloud path untouched while both transports share the same bounded relay.
type deviceFrameStream struct {
	stream agentpbv2.WendyTunnelService_DatagramTunnelServer
}

func (d deviceFrameStream) Send(msg *cloudpb.TunnelData) error {
	frame := &agentpbv2.DeviceDatagramFrame{}
	switch {
	case msg.GetDatagram() != nil:
		datagram := msg.GetDatagram()
		frame.Content = &agentpbv2.DeviceDatagramFrame_Datagram{Datagram: &agentpbv2.DeviceDatagram{
			FlowId: datagram.GetFlowId(), Port: datagram.GetPort(), Payload: datagram.GetPayload(),
		}}
	case msg.GetIcmpRequest() != nil:
		request := msg.GetIcmpRequest()
		frame.Content = &agentpbv2.DeviceDatagramFrame_IcmpRequest{IcmpRequest: &agentpbv2.DeviceIcmpEchoRequest{
			Identifier: request.GetIdentifier(), Sequence: request.GetSequence(),
			Payload: request.GetPayload(), OriginateUnixNs: request.GetOriginateUnixNs(),
		}}
	case msg.GetIcmpReply() != nil:
		reply := msg.GetIcmpReply()
		frame.Content = &agentpbv2.DeviceDatagramFrame_IcmpReply{IcmpReply: &agentpbv2.DeviceIcmpEchoReply{
			Identifier: reply.GetIdentifier(), Sequence: reply.GetSequence(),
			Payload: reply.GetPayload(), OriginateUnixNs: reply.GetOriginateUnixNs(),
			AgentUnixNs: reply.GetAgentUnixNs(),
		}}
	}
	return d.stream.Send(frame)
}

func (d deviceFrameStream) Recv() (*cloudpb.TunnelData, error) {
	frame, err := d.stream.Recv()
	if err != nil {
		return nil, err
	}

	msg := &cloudpb.TunnelData{}
	switch content := frame.GetContent().(type) {
	case *agentpbv2.DeviceDatagramFrame_Datagram:
		msg.Datagram = &cloudpb.TunnelDatagram{
			FlowId:  content.Datagram.GetFlowId(),
			Port:    content.Datagram.GetPort(),
			Payload: content.Datagram.GetPayload(),
		}
	case *agentpbv2.DeviceDatagramFrame_IcmpRequest:
		msg.IcmpRequest = &cloudpb.IcmpEchoRequest{
			Identifier:      content.IcmpRequest.GetIdentifier(),
			Sequence:        content.IcmpRequest.GetSequence(),
			Payload:         content.IcmpRequest.GetPayload(),
			OriginateUnixNs: content.IcmpRequest.GetOriginateUnixNs(),
		}
	case *agentpbv2.DeviceDatagramFrame_IcmpReply:
		msg.IcmpReply = &cloudpb.IcmpEchoReply{
			Identifier:      content.IcmpReply.GetIdentifier(),
			Sequence:        content.IcmpReply.GetSequence(),
			Payload:         content.IcmpReply.GetPayload(),
			OriginateUnixNs: content.IcmpReply.GetOriginateUnixNs(),
			AgentUnixNs:     content.IcmpReply.GetAgentUnixNs(),
		}
	}
	return msg, nil
}
