package cloudrelay

import (
	"fmt"
	"strconv"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
)

// PortService names a device-loopback port in Cloud's signed service vocabulary.
// Keep the existing TCP names so agent/SSH connections also work with older Cloud.
func PortService(protocol string, port uint32) (string, error) {
	if (protocol != "tcp" && protocol != "udp") || port == 0 || port > 65535 {
		return "", fmt.Errorf("invalid tunnel protocol or port")
	}
	if protocol == "tcp" {
		switch port {
		case 22:
			return "ssh", nil
		case 50052:
			return "wendy-agent", nil
		}
	}
	return protocol + "-" + strconv.FormatUint(uint64(port), 10), nil
}

func serviceTarget(service string) (pb.DialTransport, uint32, error) {
	switch service {
	case "ssh":
		return pb.DialTransport_DIAL_TRANSPORT_TCP, 22, nil
	case "wendy-agent":
		return pb.DialTransport_DIAL_TRANSPORT_TCP, 50052, nil
	}
	protocol, text, ok := strings.Cut(service, "-")
	port, err := strconv.ParseUint(text, 10, 16)
	if !ok || err != nil || port == 0 || strconv.FormatUint(port, 10) != text {
		return 0, 0, fmt.Errorf("unsupported Cloud service descriptor")
	}
	switch protocol {
	case "tcp":
		return pb.DialTransport_DIAL_TRANSPORT_TCP, uint32(port), nil
	case "udp":
		return pb.DialTransport_DIAL_TRANSPORT_DATAGRAM, uint32(port), nil
	default:
		return 0, 0, fmt.Errorf("unsupported Cloud service descriptor")
	}
}
