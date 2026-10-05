package cloudrelay

import (
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"testing"
)

func TestRegistryCatalogIsLoopbackAndPortBound(t *testing.T) {
	for service, port := range map[string]uint32{"wendy-registry": 5000, "wendy-registry-darwin": 5555} {
		d := &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: port, ServiceDescriptor: []byte(service)}
		if err := validateService(d); err != nil {
			t.Fatal(err)
		}
		d.Port = 22
		if err := validateService(d); err == nil {
			t.Fatal("registry redirected to SSH")
		}
		d.Port = port
		d.Host = "192.0.2.1"
		if err := validateService(d); err == nil {
			t.Fatal("registry escaped device loopback")
		}
	}
}
