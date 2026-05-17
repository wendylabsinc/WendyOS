package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

type testGPGKey struct {
	entity      *openpgp.Entity
	pubKeyArmor []byte
}

func newTestGPGKey(t *testing.T) *testGPGKey {
	t.Helper()
	entity, err := openpgp.NewEntity("Test", "", "test@test.com", nil)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := entity.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	w.Close()
	return &testGPGKey{entity: entity, pubKeyArmor: buf.Bytes()}
}

func (k *testGPGKey) sign(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP SIGNATURE", nil)
	if err != nil {
		t.Fatalf("armor.Encode sig: %v", err)
	}
	if err := openpgp.DetachSign(w, k.entity, bytes.NewReader(data), nil); err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	w.Close()
	return buf.Bytes()
}

func startUpdateV1Server(t *testing.T, pubKeyArmor []byte) (agentpb.WendyAgentServiceClient, func()) {
	t.Helper()
	buf := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	svc := NewAgentService(zap.NewNop(), &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{})
	svc.gpgPublicKey = pubKeyArmor
	svc.exitFunc = func(int) {} // no-op: tests must not call os.Exit
	agentpb.RegisterWendyAgentServiceServer(srv, svc)
	go srv.Serve(buf) //nolint:errcheck
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return buf.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return agentpb.NewWendyAgentServiceClient(conn), func() { conn.Close(); srv.Stop() }
}

func sendUpdateStream(t *testing.T, client agentpb.WendyAgentServiceClient, binary, sig []byte, skipVerify bool) error {
	t.Helper()
	stream, err := client.UpdateAgent(context.Background())
	if err != nil {
		return err
	}
	if err := stream.Send(&agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Chunk_{
			Chunk: &agentpb.UpdateAgentRequest_Chunk{Data: binary},
		},
	}); err != nil {
		return err
	}
	h := sha256.Sum256(binary)
	if err := stream.Send(&agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Control{
			Control: &agentpb.UpdateAgentRequest_ControlCommand{
				Command: &agentpb.UpdateAgentRequest_ControlCommand_Update_{
					Update: &agentpb.UpdateAgentRequest_ControlCommand_Update{
						Sha256:        hex.EncodeToString(h[:]),
						GpgSignature:  sig,
						SkipGpgVerify: skipVerify,
					},
				},
			},
		},
	}); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

func TestUpdateAgentV1_MissingSignature_Rejected(t *testing.T) {
	key := newTestGPGKey(t)
	client, cleanup := startUpdateV1Server(t, key.pubKeyArmor)
	defer cleanup()

	err := sendUpdateStream(t, client, []byte("binary"), nil, false)
	if err == nil {
		t.Fatal("expected error when signature is missing, got nil")
	}
	s, ok := status.FromError(err)
	if !ok || s.Code() != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got: %v", err)
	}
}

func TestUpdateAgentV1_InvalidSignature_Rejected(t *testing.T) {
	key := newTestGPGKey(t)
	otherKey := newTestGPGKey(t)
	client, cleanup := startUpdateV1Server(t, key.pubKeyArmor)
	defer cleanup()

	binary := []byte("binary data")
	wrongSig := otherKey.sign(t, binary)

	err := sendUpdateStream(t, client, binary, wrongSig, false)
	if err == nil {
		t.Fatal("expected error for wrong-key signature, got nil")
	}
	s, ok := status.FromError(err)
	if !ok || s.Code() != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got: %v", err)
	}
}

func TestUpdateAgentV1_SkipVerify_AcceptsUnsigned(t *testing.T) {
	key := newTestGPGKey(t)
	client, cleanup := startUpdateV1Server(t, key.pubKeyArmor)
	defer cleanup()

	err := sendUpdateStream(t, client, []byte("dev binary"), nil, true)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			if s.Code() == codes.PermissionDenied {
				t.Fatalf("skip_gpg_verify=true should not produce PermissionDenied: %v", err)
			}
		}
	}
}
