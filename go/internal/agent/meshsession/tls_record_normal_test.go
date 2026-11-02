package meshsession

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNormalTLSAppBulkDuplex(t *testing.T) {
	a, b := fixtureCredentials(t)
	port := tlsEchoApp(t)
	_, address := startTLSAppTest(t, b, &countingAuthorizer{port: port})
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var flows []net.Conn
	for range 2 {
		flow, err := client.DialTLS(context.Background(), b.Device, address, port)
		if err != nil {
			t.Fatal(err)
		}
		defer flow.Close()
		flows = append(flows, flow)
	}
	var workers sync.WaitGroup
	for index, flow := range flows {
		workers.Go(func() {
			_ = flow.SetDeadline(time.Now().Add(5 * time.Second))
			payload := bytes.Repeat([]byte{byte(index + 1)}, 65507)
			sent := make(chan error, 1)
			go func() { _, err := flow.Write(payload); sent <- err }()
			reply := make([]byte, len(payload))
			if _, err := io.ReadFull(flow, reply); err != nil {
				t.Error(err)
			}
			if err := <-sent; err != nil {
				t.Error(err)
			}
			if !bytes.Equal(payload, reply) {
				t.Error("ordinary bulk TLS echo mismatch")
			}
		})
	}
	workers.Wait()
}
