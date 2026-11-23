package rtps

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestDiscoveryHeartbeatRepliesToListeningLocator(t *testing.T) {
	for _, advertised := range []bool{true, false} {
		name := "before_spdp"
		if advertised {
			name = "separate_output_socket"
		}
		t.Run(name, func(t *testing.T) {
			p := wireParticipant(t)
			sender, _ := listener(t)
			receiver, loc := listener(t)
			peer := GUIDPrefix{1, 15, 77}
			if advertised {
				p.peers[peer] = []Locator{loc}
			} else {
				receiver = sender
			}
			body := make([]byte, 28)
			binary.BigEndian.PutUint32(body[4:8], entitySEDPPubWriter)
			binary.LittleEndian.PutUint32(body[12:16], 1)
			binary.LittleEndian.PutUint32(body[20:24], 4)
			p.handleHeartbeat(peer, Submessage{Kind: subHEARTBEAT, Endian: binary.LittleEndian, Body: body}, sender.LocalAddr().(*net.UDPAddr))
			_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 2048)
			n, _, err := receiver.ReadFromUDP(buf)
			if err != nil {
				t.Fatalf("discovery listener did not receive ACKNACK: %v", err)
			}
			msg, err := ParseMessage(buf[:n])
			if err != nil || len(msg.Submessages) != 2 || msg.Submessages[1].Kind != subACKNACK {
				t.Fatalf("unexpected reply: %+v, %v", msg, err)
			}
			if p.Stats().AcksSent != 1 {
				t.Fatalf("unexpected statistics: %+v", p.Stats())
			}
			if advertised {
				_ = sender.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
				if _, _, err := sender.ReadFromUDP(buf); err == nil {
					t.Fatal("reply also sent to output-only socket")
				}
			}
		})
	}
}
