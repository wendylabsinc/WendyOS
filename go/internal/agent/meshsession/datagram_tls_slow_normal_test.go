package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"testing"
	"time"
)

// ESP32 sends one UDP response in several valid TLS application records.
// Ordinary BLE latency can exceed the receiver's 100ms polling interval.
func TestNormalTLSDatagramSlowRecordEcho(t *testing.T) {
	for _, size := range []int{4096, 16384} {
		t.Run(fmt.Sprintf("bytes-%d", size), func(t *testing.T) {
			testNormalTLSDatagramSlowRecordEcho(t, size)
		})
	}
}

func testNormalTLSDatagramSlowRecordEcho(t *testing.T, size int) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, &countingAuthorizer{port: 28080})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	config := server.TLSConfig()
	config.NextProtos = []string{TLSALPN}
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(8 * time.Second))
		var request [8]byte
		if _, err = io.ReadFull(conn, request[:]); err != nil {
			done <- err
			return
		}
		if string(request[:4]) != "WAU3" || binary.BigEndian.Uint16(request[4:6]) != 28080 || binary.BigEndian.Uint16(request[6:]) < uint16(size) {
			done <- fmt.Errorf("ordinary UDP port request: %x", request)
			return
		}
		var ack [8]byte
		copy(ack[:4], "WUA3")
		ack[4] = 1
		ack[5] = 3
		binary.BigEndian.PutUint16(ack[6:], uint16(size))
		if _, err = conn.Write(ack[:]); err != nil {
			done <- err
			return
		}
		var header [2]byte
		if _, err = io.ReadFull(conn, header[:]); err != nil {
			done <- err
			return
		}
		payload := make([]byte, int(binary.BigEndian.Uint16(header[:])))
		if _, err = io.ReadFull(conn, payload); err != nil {
			done <- err
			return
		}
		wire := append(header[:], payload...)
		for len(wire) > 0 {
			n := min(1024, len(wire))
			if _, err = conn.Write(wire[:n]); err != nil {
				done <- err
				return
			}
			wire = wire[n:]
			time.Sleep(180 * time.Millisecond)
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	address := netip.MustParseAddrPort(listener.Addr().String())
	flow, err := DialUDPTLS(ctx, a, b.Device, address, 28080)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	payload := bytes.Repeat([]byte{0x31, 0xa7}, size/2)
	if err = flow.Send(payload); err != nil {
		t.Fatal(err)
	}
	reply, err := flow.Receive(ctx)
	if err != nil {
		t.Fatal("valid delayed TLS response", err)
	}
	defer reply.Release()
	if !bytes.Equal(reply.Payload, payload) {
		t.Fatalf("echo length=%d", len(reply.Payload))
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
