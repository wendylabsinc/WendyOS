//go:build esp32lab

package localmesh

import (
	"bytes"
	"encoding/hex"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"os"
	"os/exec"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func TestNativeRouterControlBinaryInteroperability(t *testing.T) {
	binary := os.Getenv("WENDY_NATIVE_CONTROL_ORACLE")
	if binary == "" {
		t.Skip("set WENDY_NATIVE_CONTROL_ORACLE to the native net_control_test executable")
	}
	payload := make([]byte, netgossip.MaxObject)
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}
	id := netgossip.Identify(netgossip.Manifest, payload)
	hash := exec.Command(binary, "hash")
	hash.Stdin = bytes.NewReader(payload)
	got, err := hash.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes.TrimSpace(got)) != hex.EncodeToString(id.Hash[:]) {
		t.Fatalf("native artifact digest differs: %q", got)
	}
	for _, op := range []string{"have", "want", "unavailable", "data", "gone"} {
		m := &netgossip.Message{Version: 1, Type: netgossip.Manifest, Op: op, Digest: id.Hash[:]}
		if op != "have" && op != "gone" {
			m.Request = 1<<63 + 17
		}
		if op == "data" {
			m.Offset = netgossip.ChunkSize
			m.Total = len(payload)
			m.Data = payload[m.Offset : m.Offset+netgossip.ChunkSize]
		}
		input, err := netwire.Marshal(ControlMessage{Kind: "gossip", Gossip: m})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "wire")
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", op, err, output)
		}
		var wire bytes.Buffer
		var result ControlMessage
		if err = netwire.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		if err = WriteControl(&wire, result); err != nil {
			t.Fatal(err)
		}
		result, err = ReadControl(&wire)
		if err != nil {
			t.Fatal(err)
		}
		n := result.Gossip
		if n == nil || n.Op != m.Op || n.Request != m.Request || n.Offset != m.Offset || n.Total != m.Total || !bytes.Equal(n.Digest, m.Digest) || !bytes.Equal(n.Data, m.Data) {
			t.Fatalf("%s: altered native wire: %+v", op, n)
		}
	}
}

func TestNativeCertificateBinaryInteroperability(t *testing.T) {
	oracle := os.Getenv("WENDY_NATIVE_CONTROL_ORACLE")
	if oracle == "" {
		t.Skip("set native control oracle")
	}
	principal := "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/00000000-0000-4000-8000-000000000004"
	hash := hex.EncodeToString(bytes.Repeat([]byte{42}, 32))
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i * 17)
	}
	messages := []*CertificateMessage{
		{Chain: &CertificateChain{Principal: principal, Hashes: []string{hash}}},
		{Request: &CertificateRequest{Hash: hash, Offset: 1024}},
		{Chunk: &CertificateChunk{Hash: hash, Offset: 1024, Total: 2793, Data: payload}},
	}
	for _, certificate := range messages {
		wire, err := netwire.Marshal(ControlMessage{Kind: "certificate", Certificate: certificate})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(oracle, "certificate")
		cmd.Stdin = bytes.NewReader(wire)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(output))
		}
		// Both encoders use identical deterministic bytes, including raw hashes.
		if !bytes.Equal(wire, output) {
			t.Fatalf("binary certificate differs: Go=%x C=%x", wire, output)
		}
		var frame bytes.Buffer
		frame.Write([]byte{byte(len(output) >> 24), byte(len(output) >> 16), byte(len(output) >> 8), byte(len(output))})
		frame.Write(output)
		decoded, err := ReadControl(&frame)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Certificate == nil {
			t.Fatal("missing certificate")
		}
	}
}
