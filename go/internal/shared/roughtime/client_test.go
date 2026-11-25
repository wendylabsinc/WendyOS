package roughtime_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

// startFakeRoughtimeServer starts a UDP server that signs a real Roughtime
// response for the given nonce using the provided private key.
func startFakeRoughtimeServer(t *testing.T, privKey ed25519.PrivateKey, midpMicros uint64, radiMicros uint32) (addr string, stop func()) {
	t.Helper()
	_, onlinePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("online keygen: %v", err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, remote, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			reqBytes := buf[:n]
			if n < 1024 {
				continue
			}
			if len(reqBytes) < 12 || string(reqBytes[:8]) != "ROUGHTIM" {
				continue
			}
			msgLen := binary.LittleEndian.Uint32(reqBytes[8:12])
			if int(msgLen) != len(reqBytes)-12 {
				continue
			}
			req, err := roughtime.DecodeMessage(reqBytes[12:])
			if err != nil {
				continue
			}
			nonce := req[roughtime.TagNONC]
			if len(nonce) == 0 {
				continue
			}

			// Build SREP
			midpB := make([]byte, 8)
			binary.LittleEndian.PutUint64(midpB, midpMicros/1_000_000)
			radiB := make([]byte, 4)
			binary.LittleEndian.PutUint32(radiB, radiMicros/1_000_000)
			// ROOT = SHA-512("\x00" || nonce) truncated to 32 bytes for a single-client batch.
			leaf := sha512.Sum512(append([]byte{0x00}, nonce...))
			verB := make([]byte, 4)
			binary.LittleEndian.PutUint32(verB, roughtime.VersionDraft08)
			srep := roughtime.EncodeMessage(map[uint32][]byte{
				roughtime.TagMIDP: midpB,
				roughtime.TagRADI: radiB,
				roughtime.TagROOT: leaf[:32],
			})

			// Sign: context || SREP
			toSign := append([]byte(roughtime.SigContext), srep...)
			sig := ed25519.Sign(onlinePriv, toSign)

			pubk := ed25519.PrivateKey(onlinePriv).Public().(ed25519.PublicKey)
			mintB := make([]byte, 8)
			binary.LittleEndian.PutUint64(mintB, 0)
			maxtB := make([]byte, 8)
			binary.LittleEndian.PutUint64(maxtB, ^uint64(0))
			dele := roughtime.EncodeMessage(map[uint32][]byte{
				roughtime.TagMINT: mintB,
				roughtime.TagMAXT: maxtB,
				roughtime.TagPUBK: pubk,
			})
			certSig := ed25519.Sign(privKey, append([]byte(roughtime.CertContext), dele...))
			cert := roughtime.EncodeMessage(map[uint32][]byte{
				roughtime.TagSIG:  certSig,
				roughtime.TagDELE: dele,
			})

			indxB := make([]byte, 4) // index 0
			resp := roughtime.EncodeMessage(map[uint32][]byte{
				roughtime.TagCERT: cert,
				roughtime.TagNONC: nonce,
				roughtime.TagSIG:  sig,
				roughtime.TagSREP: srep,
				roughtime.TagINDX: indxB,
				roughtime.TagPATH: {},
				roughtime.TagVER:  verB,
			})
			framed := append([]byte("ROUGHTIM"), make([]byte, 4)...)
			binary.LittleEndian.PutUint32(framed[8:12], uint32(len(resp)))
			framed = append(framed, resp...)
			pc.WriteTo(framed, remote) //nolint:errcheck
		}
	}()
	return pc.LocalAddr().String(), func() { pc.Close() }
}

func TestQuery_ValidResponse(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	wantTime := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	midpMicros := uint64(wantTime.UnixMicro())

	addr, stop := startFakeRoughtimeServer(t, priv, midpMicros, 500_000)
	defer stop()

	srv := roughtime.Server{Name: "test", Address: addr, PublicKey: pub}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := roughtime.Query(ctx, []roughtime.Server{srv})
	if err != nil {
		t.Fatalf("Query error: %v", err)
	}
	if result.Server != "test" {
		t.Errorf("Server: got %q want %q", result.Server, "test")
	}
	diff := result.Midpoint.Sub(wantTime).Abs()
	if diff > time.Second {
		t.Errorf("Midpoint off by %v", diff)
	}
	localHost, localPort, err := net.SplitHostPort(result.LocalAddress)
	if err != nil || localHost != "127.0.0.1" || localPort == "0" {
		t.Fatalf("direct local socket path not recorded: %q %v", result.LocalAddress, err)
	}
	if result.RemoteAddress != addr {
		t.Fatalf("direct remote socket path=%q want%q", result.RemoteAddress, addr)
	}
	// Re-verifying the signed bytes cannot reconstruct or authenticate a
	// socket observation; transport metadata must not become proof data.
	verified, err := roughtime.VerifyResponse(result.RawResponse, result.Nonce, srv)
	if err != nil || verified.LocalAddress != "" || verified.RemoteAddress != "" {
		t.Fatalf("transport path leaked into authority proof: %+v %v", verified, err)
	}
}

func TestQuery_BadSignature(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader) // different key

	addr, stop := startFakeRoughtimeServer(t, priv, uint64(time.Now().UnixMicro()), 1_000_000)
	defer stop()

	srv := roughtime.Server{Name: "test", Address: addr, PublicKey: wrongPub}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := roughtime.Query(ctx, []roughtime.Server{srv}); err == nil {
		t.Fatal("expected error for bad signature")
	}
}

func TestQuery_RadiusTooLarge(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	addr, stop := startFakeRoughtimeServer(t, priv, uint64(time.Now().UnixMicro()), 11_000_000) // 11s > 10s limit
	defer stop()

	srv := roughtime.Server{Name: "test", Address: addr, PublicKey: pub}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := roughtime.Query(ctx, []roughtime.Server{srv}); err == nil {
		t.Fatal("expected error for radius exceeding limit")
	}
}

func TestVerifyResponse_TamperedPayload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	addr, stop := startFakeRoughtimeServer(t, priv, uint64(time.Now().UnixMicro()), 1_000_000)
	defer stop()

	srv := roughtime.Server{Name: "test", Address: addr, PublicKey: pub}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := roughtime.Query(ctx, []roughtime.Server{srv})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	// Tamper with the raw response
	tampered := make([]byte, len(result.RawResponse))
	copy(tampered, result.RawResponse)
	tampered[len(tampered)/2] ^= 0xFF

	nonce := make([]byte, 64) // wrong nonce — doesn't matter, sig check fires first
	if _, err := roughtime.VerifyResponse(tampered, nonce, srv); err == nil {
		t.Fatal("expected error for tampered response")
	}
}
