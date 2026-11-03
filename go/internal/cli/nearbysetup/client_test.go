package nearbysetup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

func readTestFrame(c net.Conn, message proto.Message) error {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	b := make([]byte, binary.BigEndian.Uint16(h[:]))
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	return proto.Unmarshal(b, message)
}
func writeTestFrame(c net.Conn, message proto.Message) error {
	b, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	b = append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)
	// Force headers and protobuf fields to cross stream reads.
	for _, v := range b {
		if _, err := c.Write([]byte{v}); err != nil {
			return err
		}
	}
	return nil
}
func setupTestPeer(c net.Conn, mode string) error {
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	hello := new(pb.BluetoothCommand)
	if err := readTestFrame(c, hello); err != nil {
		return err
	}
	public, err := ecdh.X25519().NewPublicKey(hello.GetSetupHello().GetPublicKey())
	if err != nil {
		return err
	}
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	secret, _ := key.ECDH(public)
	keys, _ := hkdf.Key(sha256.New, secret, append(public.Bytes(), key.PublicKey().Bytes()...), "wendy-ble-setup-v1", 64)
	rb, _ := aes.NewCipher(keys[:32])
	read, _ := cipher.NewGCM(rb)
	wb, _ := aes.NewCipher(keys[32:])
	write, _ := cipher.NewGCM(wb)
	if err := writeTestFrame(c, &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupHello{SetupHello: &pb.SetupHello{PublicKey: key.PublicKey().Bytes()}}}); err != nil {
		return err
	}
	var first []byte
	for counter := uint64(1); counter <= 2; counter++ {
		command := new(pb.BluetoothCommand)
		if err := readTestFrame(c, command); err != nil {
			return err
		}
		data := command.GetSetupSealed().GetData()
		nonce := binary.BigEndian.AppendUint64(make([]byte, 4), counter)
		if len(data) < 28 || !bytes.Equal(data[:12], nonce) {
			return errors.New("unsealed or out-of-order command")
		}
		plain, err := read.Open(nil, nonce, data[12:], nil)
		if err != nil {
			return err
		}
		inner := new(pb.BluetoothCommand)
		if err := proto.Unmarshal(plain, inner); err != nil {
			return err
		}
		if inner.GetSetupStatus() == nil || inner.GetSetupStatus().GetRequestNonce() != (counter == 2) {
			return errors.New("unexpected status/nonce command")
		}
		reply := &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupStatus{SetupStatus: &pb.SetupStatusResponse{DeviceClass: "C", LiteSupported: true, MaxConfigSize: 4096}}}
		body, _ := proto.Marshal(reply)
		sealed := write.Seal(bytes.Clone(nonce), nonce, body, nil)
		if counter == 1 {
			first = bytes.Clone(sealed)
		}
		if counter == 2 {
			switch mode {
			case "tamper":
				sealed[len(sealed)-1] ^= 1
			case "replay":
				sealed = first
			case "plain":
				return writeTestFrame(c, reply)
			}
		}
		if err := writeTestFrame(c, &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupSealed{SetupSealed: &pb.SetupSealed{Data: sealed}}}); err != nil {
			return err
		}
	}
	return nil
}
func TestEncryptedSetupRoundtripAndRejectInvalidReplies(t *testing.T) {
	for _, mode := range []string{"valid", "tamper", "replay", "plain"} {
		t.Run(mode, func(t *testing.T) {
			wire, peer := net.Pipe()
			done := make(chan error, 1)
			go func() { done <- setupTestPeer(peer, mode) }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := Open(ctx, wire)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s, err := c.Status(ctx, false)
			if err != nil || !s.GetLiteSupported() {
				t.Fatalf("status: %v, %v", s, err)
			}
			// Management calls are rejected without consuming a sequence number.
			if _, err := c.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_AppsList{AppsList: &pb.AppsListCommand{}}}); err == nil {
				t.Fatal("accepted management operation")
			}
			_, err = c.Status(ctx, true)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("mode %s error: %v", mode, err)
			}
			if peerErr := <-done; peerErr != nil {
				t.Fatal(peerErr)
			}
			if mode != "valid" {
				if _, err := wire.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("failed session remained open: %v", err)
				}
			}
		})
	}
}
func TestHandshakeCancellationClosesConnection(t *testing.T) {
	wire, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	go func() { var hello pb.BluetoothCommand; _ = readTestFrame(peer, &hello); close(ready); cancel() }()
	_, err := Open(ctx, wire)
	if err == nil {
		t.Fatal("stalled handshake succeeded")
	}
	<-ready
	if _, err := peer.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("cancel did not close connection: %v", err)
	}
}

// This fixture is shared with the agent, Lite firmware and Swift companion SDK.
func TestSetupWireCompatibility(t *testing.T) {
	raw, err := os.ReadFile("../../agent/bluetooth/testdata/setup-wire-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]string
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	decode := func(key string) []byte {
		v, err := hex.DecodeString(fixture[key])
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	private := make([]byte, 32)
	for i := range private {
		private[i] = byte(i + 1)
	}
	key, err := ecdh.X25519().NewPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	wire, peer := net.Pipe()
	defer peer.Close()
	done := make(chan error, 1)
	go func() {
		var command pb.BluetoothCommand
		if err := readTestFrame(peer, &command); err != nil {
			done <- err
			return
		}
		if hex.EncodeToString(command.GetSetupHello().GetPublicKey()) != fixture["client"] {
			done <- errors.New("unexpected client key")
			return
		}
		public, _ := hex.DecodeString(fixture["server"])
		done <- writeTestFrame(peer, &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupHello{SetupHello: &pb.SetupHello{PublicKey: public}}})
	}()
	c := &Client{wire: wire}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.handshake(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	nonce := binary.BigEndian.AppendUint64(make([]byte, 4), 1)
	request := c.write.Seal(bytes.Clone(nonce), nonce, []byte("wifi password"), nil)
	if !bytes.Equal(request, decode("request")) {
		t.Fatal("incompatible encrypted request")
	}
	response := decode("response")
	plain, err := c.read.Open(nil, response[:12], response[12:], nil)
	if err != nil || string(plain) != "connected" {
		t.Fatal("incompatible encrypted reply", err)
	}
}
