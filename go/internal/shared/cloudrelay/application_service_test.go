package cloudrelay

import (
	"context"
	"net"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
)

func TestApplicationServiceRequiresDedicatedAdmission(t *testing.T) {
	instruction := &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: applicationServicePort, ServiceDescriptor: []byte(ApplicationService)}
	if err := validateService(instruction); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{MTLSPort: 50052}
	if conn, err := agent.dialInstruction(context.Background(), instruction, 50052); err == nil {
		conn.Close()
		t.Fatal("app service reached an ordinary socket without application admission")
	}
	called := false
	agent.ApplicationDial = func(context.Context) (net.Conn, error) {
		called = true
		local, remote := net.Pipe()
		remote.Close()
		return local, nil
	}
	conn, err := agent.dialInstruction(context.Background(), instruction, 50052)
	if err != nil || !called {
		t.Fatalf("dedicated application admission=%v called=%v", err, called)
	}
	conn.Close()
	for _, port := range []uint32{22, 50052, applicationServicePort + 1} {
		instruction.Port = port
		called = false
		if _, err := agent.dialInstruction(context.Background(), instruction, int(port)); err == nil || called {
			t.Fatalf("mutated app endpoint %d reached callback", port)
		}
	}
}

func TestApplicationServiceUnsupportedExplainsBackendRollout(t *testing.T) {
	message := (&UnsupportedServiceError{Service: ApplicationService}).Error()
	if !strings.Contains(message, "device-originated") || !strings.Contains(message, "backend") {
		t.Fatalf("unclear app-only capability error: %s", message)
	}
}
