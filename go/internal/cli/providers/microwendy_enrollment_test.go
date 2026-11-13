package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"google.golang.org/grpc"
)

func TestConnectEnrollmentRejectsNetworkTransports(t *testing.T) {
	oldDialer := CloudRelayDialer
	t.Cleanup(func() { CloudRelayDialer = oldDialer })
	CloudRelayDialer = func(context.Context, models.ExternalDevice, bool) (*grpc.ClientConn, context.Context, liteclient.RelayOptions, error) {
		t.Fatal("enrollment must not open a Cloud relay")
		return nil, nil, liteclient.RelayOptions{}, nil
	}
	for _, transport := range []string{"LAN", "BLE", "Cloud", "", "usb", "UART"} {
		t.Run(transport, func(t *testing.T) {
			client, err := (&MicroWendyProvider{}).ConnectEnrollment(models.ExternalDevice{
				ConnectionInfo: map[string]string{"type": transport},
			})
			if client != nil {
				client.Close()
				t.Fatal("enrollment opened a network client")
			}
			if err == nil || !strings.Contains(err.Error(), "requires a USB connection") {
				t.Fatalf("expected USB requirement before transport handling, got %v", err)
			}
		})
	}
}

func TestConnectEnrollmentUsesUSBSerialTransport(t *testing.T) {
	oldConnect := connectSerialFn
	t.Cleanup(func() { connectSerialFn = oldConnect })
	var connected bool
	connectSerialFn = func(_ context.Context, _ *liteclient.WendyLiteClient, port string) error {
		if port != "/dev/cu.usbmodem-test" {
			t.Fatalf("unexpected enrollment serial port %q", port)
		}
		connected = true
		return nil
	}
	client, err := (&MicroWendyProvider{}).ConnectEnrollment(models.ExternalDevice{
		ConnectionInfo: map[string]string{
			"type":       "USB",
			"serialPort": "/dev/cu.usbmodem-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if !connected {
		t.Fatal("enrollment did not open the USB serial connection")
	}
}
