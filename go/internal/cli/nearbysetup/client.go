// Package nearbysetup implements the restricted, encrypted first-enrollment
// service. It never opens the management or mesh PSM.
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
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/central"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

const ServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b632"
const PSM = 130

type Client struct {
	wire        net.Conn
	mu          sync.Mutex
	closeOnce   sync.Once
	read, write cipher.AEAD
	counter     uint64
}

func Connect(ctx context.Context, address string) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := central.Connect(address, 15)
	if err != nil {
		return nil, errors.New("Bluetooth connection failed; keep the device nearby and retry")
	}
	if err = conn.OpenL2CAP(PSM, 15); err != nil {
		conn.Close()
		return nil, errors.New("Could not open Bluetooth setup; check the device firmware and retry")
	}
	return Open(ctx, central.NewL2CAPStream(conn))
}

// Open owns wire, including on failed handshakes.
func Open(ctx context.Context, wire net.Conn) (*Client, error) {
	c := &Client{wire: wire}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err == nil {
		err = c.handshake(ctx, key)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *Client) Close() { c.closeOnce.Do(func() { _ = c.wire.Close() }) }

func (c *Client) handshake(ctx context.Context, key *ecdh.PrivateKey) error {
	r, err := c.exchange(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_SetupHello{SetupHello: &pb.SetupHello{PublicKey: key.PublicKey().Bytes()}}})
	if err != nil {
		return err
	}
	peer, err := ecdh.X25519().NewPublicKey(r.GetSetupHello().GetPublicKey())
	if err != nil {
		return errors.New("Device does not support encrypted setup")
	}
	secret, err := key.ECDH(peer)
	if err != nil {
		return errors.New("Invalid setup key")
	}
	defer clear(secret)
	material, err := hkdf.Key(sha256.New, secret, append(key.PublicKey().Bytes(), peer.Bytes()...), "wendy-ble-setup-v1", 64)
	if err != nil {
		return err
	}
	defer clear(material)
	w, _ := aes.NewCipher(material[:32])
	rblock, _ := aes.NewCipher(material[32:])
	c.write, _ = cipher.NewGCM(w)
	c.read, _ = cipher.NewGCM(rblock)
	return nil
}

// Call serializes request/response pairs. An uncertain exchange closes the
// session: continuing could associate a late reply with a different command.
func (c *Client) Call(ctx context.Context, command *pb.BluetoothCommand) (reply *pb.BluetoothResponse, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch command.Command.(type) {
	case *pb.BluetoothCommand_SetupStatus, *pb.BluetoothCommand_WifiList, *pb.BluetoothCommand_WifiStatus, *pb.BluetoothCommand_WifiConnect, *pb.BluetoothCommand_SetupEnroll:
	default:
		return nil, errors.New("Command unavailable during setup")
	}
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	if c.counter == ^uint64(0) {
		return nil, errors.New("Setup session expired")
	}
	plain, err := proto.Marshal(command)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	c.counter++
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], c.counter)
	sealed := c.write.Seal(bytes.Clone(nonce), nonce, plain, nil)
	outer, err := c.exchange(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_SetupSealed{SetupSealed: &pb.SetupSealed{Data: sealed}}})
	if err != nil {
		return nil, err
	}
	data := outer.GetSetupSealed().GetData()
	if len(data) < 28 || !bytes.Equal(data[:12], nonce) {
		return nil, errors.New("Invalid encrypted setup response")
	}
	plainReply, err := c.read.Open(nil, nonce, data[12:], nil)
	if err != nil {
		return nil, errors.New("Setup response authentication failed")
	}
	defer clear(plainReply)
	reply = new(pb.BluetoothResponse)
	if err = proto.Unmarshal(plainReply, reply); err != nil {
		return nil, errors.New("Invalid setup response")
	}
	// Do not expose raw device errors: they may include credentials or network input.
	if reply.GetError() != nil {
		return reply, nil
	}
	return reply, nil
}
func (c *Client) Status(ctx context.Context, nonce bool) (*pb.SetupStatusResponse, error) {
	r, err := c.Call(ctx, &pb.BluetoothCommand{Command: &pb.BluetoothCommand_SetupStatus{SetupStatus: &pb.SetupStatusCommand{RequestNonce: nonce}}})
	if err != nil {
		return nil, err
	}
	if r.GetSetupStatus() == nil {
		return nil, errors.New("Could not read device setup status")
	}
	return r.GetSetupStatus(), nil
}
func (c *Client) exchange(ctx context.Context, command *pb.BluetoothCommand) (*pb.BluetoothResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, c.Close)
	defer stop()
	deadline := time.Now().Add(80 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.wire.SetDeadline(deadline)
	body, err := proto.Marshal(command)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	if len(body) == 0 || len(body) > 65535 {
		return nil, errors.New("Setup request is too large")
	}
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	frame = append(frame, body...)
	defer clear(frame)
	if _, err = io.Copy(c.wire, bytes.NewReader(frame)); err != nil {
		return nil, errors.New("Bluetooth connection ended; retry setup")
	}
	var length [2]byte
	if _, err = io.ReadFull(c.wire, length[:]); err != nil {
		return nil, errors.New("Bluetooth connection ended; retry setup")
	}
	n := binary.BigEndian.Uint16(length[:])
	if n == 0 {
		return nil, errors.New("Empty setup response")
	}
	data := make([]byte, n)
	if _, err = io.ReadFull(c.wire, data); err != nil {
		return nil, errors.New("Bluetooth connection ended; retry setup")
	}
	defer clear(data)
	r := new(pb.BluetoothResponse)
	if err = proto.Unmarshal(data, r); err != nil {
		return nil, errors.New("Invalid setup response")
	}
	return r, nil
}
