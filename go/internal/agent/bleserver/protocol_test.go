package bleserver

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

// seqpacketPipe creates a pair of connected net.Conn that simulate SOCK_SEQPACKET behavior.
// We use net.Pipe which preserves message boundaries (each Write produces one Read).
func seqpacketPipe() (net.Conn, net.Conn) {
	return net.Pipe()
}

func TestWriteReadRoundTrip(t *testing.T) {
	c1, c2 := seqpacketPipe()
	defer c1.Close()
	defer c2.Close()

	payload := []byte("hello, BLE!")

	errCh := make(chan error, 1)
	go func() {
		errCh <- writeMessage(c1, payload)
	}()

	got, err := readMessage(c2)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestWriteReadEmpty(t *testing.T) {
	c1, c2 := seqpacketPipe()
	defer c1.Close()
	defer c2.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- writeMessage(c1, []byte{})
	}()

	got, err := readMessage(c2)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(got))
	}
}

func TestReadMessageTooShort(t *testing.T) {
	c1, c2 := seqpacketPipe()
	defer c1.Close()
	defer c2.Close()

	// Write a single byte (less than the 2-byte header)
	go func() {
		c1.Write([]byte{0x01})
	}()

	_, err := readMessage(c2)
	if err == nil {
		t.Fatal("expected error for short message")
	}
}

func TestReadMessageLengthMismatch(t *testing.T) {
	c1, c2 := seqpacketPipe()
	defer c1.Close()
	defer c2.Close()

	// Write a frame where the length header claims more data than present
	frame := make([]byte, 4)                  // 2 header + 2 data bytes
	binary.BigEndian.PutUint16(frame[:2], 10) // claims 10 bytes
	frame[2] = 0xAA
	frame[3] = 0xBB

	go func() {
		c1.Write(frame)
	}()

	_, err := readMessage(c2)
	if err == nil {
		t.Fatal("expected error for length mismatch")
	}
}

func TestWriteMessageTooLarge(t *testing.T) {
	data := make([]byte, 65536) // exceeds uint16 max
	err := writeMessage(nil, data)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

func TestWriteReadMultipleMessages(t *testing.T) {
	c1, c2 := seqpacketPipe()
	defer c1.Close()
	defer c2.Close()

	messages := [][]byte{
		[]byte("first"),
		[]byte("second message"),
		[]byte("third"),
	}

	go func() {
		for _, msg := range messages {
			if err := writeMessage(c1, msg); err != nil {
				t.Errorf("writeMessage: %v", err)
				return
			}
		}
	}()

	for i, want := range messages {
		got, err := readMessage(c2)
		if err != nil {
			t.Fatalf("readMessage[%d]: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("message[%d]: got %q, want %q", i, got, want)
		}
	}
}
