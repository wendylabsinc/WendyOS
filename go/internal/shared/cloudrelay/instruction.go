package cloudrelay

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"

	"github.com/cloudflare/circl/hpke"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/protobuf/proto"
)

func decryptInstruction(e *pb.EncryptedDialInstructionEnvelope, c claims, lease claims, key *ecdsa.PrivateKey) (*pb.DialInstruction, error) {
	if e == nil || e.Suite != pb.HpkeSuite_HPKE_SUITE_DHKEM_P256_HKDF_SHA256_HKDF_SHA256_AES_128_GCM || len(e.EncapsulatedKey) != 65 || len(e.Ciphertext) != 4112 {
		return nil, fmt.Errorf("invalid encrypted dial envelope")
	}
	// These domains remain v1 in the frozen crypto contract even though RPCs
	// and UUID-bearing messages use the wendycloud.tunnel.v2 package.
	info := canonical([]byte("wendycloud.tunnel.v1/hpke-info/1"), []byte(c.Session), []byte(c.Route), digest(publicDER(key)))
	hash, err := decode64(c.Request)
	if err != nil {
		return nil, err
	}
	aad := canonical([]byte("wendycloud.tunnel.v1/hpke-aad/1"), []byte(c.Session), hash)
	if !bytes.Equal(info, e.Info) || !bytes.Equal(aad, e.Aad) {
		return nil, fmt.Errorf("dial envelope context mismatch")
	}
	envelope := canonical([]byte("wendycloud.tunnel.v1/hpke-envelope/1"), []byte("DHKEM(P-256,HKDF-SHA256)+HKDF-SHA256+AES-128-GCM"), e.EncapsulatedKey, info, aad, e.Ciphertext)
	if binding(envelope) != c.Dial || binding(publicDER(key)) != lease.Encryption {
		return nil, fmt.Errorf("dial envelope binding mismatch")
	}
	raw := key.D.FillBytes(make([]byte, 32))
	sk, err := hpke.KEM_P256_HKDF_SHA256.Scheme().UnmarshalBinaryPrivateKey(raw)
	clear(raw)
	if err != nil {
		return nil, err
	}
	suite := hpke.NewSuite(hpke.KEM_P256_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	receiver, err := suite.NewReceiver(sk, info)
	if err != nil {
		return nil, err
	}
	opener, err := receiver.Setup(e.EncapsulatedKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := opener.Open(e.Ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypting dial instruction: %w", err)
	}
	if len(plaintext) != 4096 {
		return nil, fmt.Errorf("invalid dial instruction size")
	}
	var d pb.DialInstruction
	if proto.Unmarshal(plaintext, &d) != nil || len(d.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("invalid dial instruction")
	}
	for _, dst := range d.AuthorizedDatagramDestinations {
		if len(dst.ProtoReflect().GetUnknown()) != 0 {
			return nil, fmt.Errorf("unknown dial destination fields")
		}
	}
	canonicalBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&d)
	if err != nil || !bytes.Equal(plaintext, canonicalBytes) {
		return nil, fmt.Errorf("noncanonical dial instruction")
	}
	if d.SessionId != c.Session || d.RelayExpiresAtUnixSeconds != c.RelayExp || len(d.Padding) == 0 {
		return nil, fmt.Errorf("dial instruction session mismatch")
	}
	if len(d.ServiceDescriptor) == 0 || len(d.ServiceDescriptor) > 2048 {
		return nil, fmt.Errorf("invalid service descriptor")
	}
	switch d.Transport {
	case pb.DialTransport_DIAL_TRANSPORT_TCP:
		if len(d.Host) == 0 || len(d.Host) > 255 || d.Port == 0 || d.Port > 65535 || len(d.AuthorizedDatagramDestinations) != 0 {
			return nil, fmt.Errorf("invalid TCP instruction")
		}
	case pb.DialTransport_DIAL_TRANSPORT_DATAGRAM:
		if d.Host != "" || d.Port != 0 || len(d.AuthorizedDatagramDestinations) == 0 || len(d.AuthorizedDatagramDestinations) > 32 {
			return nil, fmt.Errorf("invalid datagram instruction")
		}
		var last uint32
		ports := map[uint32]bool{}
		for _, dst := range d.AuthorizedDatagramDestinations {
			if dst.DestinationId <= last || dst.LoopbackPort == 0 || dst.LoopbackPort > 65535 || ports[dst.LoopbackPort] {
				return nil, fmt.Errorf("invalid datagram destination")
			}
			last = dst.DestinationId
			ports[dst.LoopbackPort] = true
		}
	default:
		return nil, fmt.Errorf("unsupported Cloud dial transport")
	}
	return &d, nil
}

func validateService(d *pb.DialInstruction) error {
	transport, port, err := serviceTarget(string(d.ServiceDescriptor))
	if err != nil {
		return err
	}
	if d.Transport != transport {
		return fmt.Errorf("cloud service transport mismatch")
	}
	if transport == pb.DialTransport_DIAL_TRANSPORT_DATAGRAM {
		if d.Host != "" || d.Port != 0 || len(d.AuthorizedDatagramDestinations) != 1 {
			return fmt.Errorf("invalid UDP service target")
		}
		dst := d.AuthorizedDatagramDestinations[0]
		if dst.GetDestinationId() != 1 || dst.GetLoopbackPort() != port {
			return fmt.Errorf("cloud UDP service destination mismatch")
		}
		return nil
	}
	if d.Port != port || len(d.AuthorizedDatagramDestinations) != 0 {
		return fmt.Errorf("cloud TCP service port mismatch")
	}
	// Cloud's catalog seals exactly 127.0.0.1 for every TCP service.
	if d.Host != "127.0.0.1" {
		return fmt.Errorf("cloud dial target is not 127.0.0.1")
	}
	return nil
}
