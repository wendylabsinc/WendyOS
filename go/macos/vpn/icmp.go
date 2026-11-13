package vpn

import (
	"context"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type echoRequest struct {
	address string
	reply   []byte
}

// Echoes to synthetic device addresses are agent reachability probes, like
// `wendy cloud ping`. Never let the local stack auto-reply for an offline device.
// Fixed workers and a bounded queue keep a ping flood from spawning unbounded
// authenticated device connections. Fragmented and oversized echoes are dropped.
func (e *Engine) startEchoWorkers() {
	e.echoes = make(chan echoRequest, 16)
	for range 8 {
		e.echoWG.Add(1)
		go func() {
			defer e.echoWG.Done()
			for {
				select {
				case <-e.ctx.Done():
					return
				case request := <-e.echoes:
					if e.ctx.Err() != nil {
						return
					}
					ctx, cancel := context.WithTimeout(e.ctx, echoTimeout)
					err := ProbeRelay(ctx, e.cfg, request.address)
					valid := err == nil && ctx.Err() == nil
					cancel()
					if !valid {
						continue
					}
					packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(request.reply)})
					var packets stack.PacketBufferList
					packets.PushBack(packet)
					e.link.WritePackets(packets)
					packet.DecRef()
				}
			}
		}()
	}
}

func (e *Engine) queueEcho(packet []byte) {
	if e.ctx.Err() != nil || len(packet) > 1400 {
		return
	}
	ip := header.IPv4(packet)
	if !ip.IsValid(len(packet)) || !ip.IsChecksumValid() || ip.TTL() == 0 ||
		ip.FragmentOffset() != 0 || ip.Flags()&header.IPv4FlagMoreFragments != 0 ||
		ip.SourceAddress().String() != TunnelAddress || !e.allowed(ip.DestinationAddress().String()) {
		return
	}
	body := packet[ip.HeaderLength():ip.TotalLength()]
	if len(body) < header.ICMPv4MinimumSize || body[0] != uint8(header.ICMPv4Echo) || body[1] != 0 || checksum.Checksum(body, 0) != 0xffff {
		return
	}
	reply := make([]byte, header.IPv4MinimumSize+len(body))
	replyIP := header.IPv4(reply)
	replyIP.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(reply)), TTL: 64, Protocol: uint8(header.ICMPv4ProtocolNumber),
		SrcAddr: ip.DestinationAddress(), DstAddr: ip.SourceAddress(),
	})
	replyIP.SetChecksum(^replyIP.CalculateChecksum())
	copy(reply[header.IPv4MinimumSize:], body)
	echo := header.ICMPv4(reply[header.IPv4MinimumSize:])
	echo.SetType(header.ICMPv4EchoReply)
	echo.SetChecksum(0)
	echo.SetChecksum(^checksum.Checksum(echo, 0))
	select {
	case e.echoes <- echoRequest{address: ip.DestinationAddress().String(), reply: reply}:
	default:
	}
}
