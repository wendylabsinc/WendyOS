package roughtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	maxRadiusMicros = 10_000_000 // 10 seconds
	minRequestSize  = 1024
	nonceSize       = 32
	udpBufSize      = 4096
	ietfFrameMagic  = "ROUGHTIM"
)

// Server is a Roughtime server descriptor with an embedded public key.
type Server struct {
	Name      string
	Address   string // "host:port"
	PublicKey ed25519.PublicKey
}

// Result is a verified Roughtime result.
type Result struct {
	Midpoint    time.Time
	Radius      time.Duration
	Server      string
	Nonce       []byte
	RawResponse []byte // raw server bytes; used for multicast relay
}

// Query queries all servers concurrently and returns the first valid result.
// The context deadline applies to each individual UDP exchange.
func Query(ctx context.Context, servers []Server) (Result, error) {
	type outcome struct {
		server string
		r      Result
		err    error
	}
	ch := make(chan outcome, len(servers))
	for _, s := range servers {
		s := s
		go func() {
			r, err := queryOne(ctx, s)
			ch <- outcome{server: s.Name, r: r, err: err}
		}()
	}
	errs := make([]string, 0, len(servers))
	for range servers {
		o := <-ch
		if o.err == nil {
			return o.r, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", o.server, o.err))
	}
	return Result{}, fmt.Errorf("all Roughtime servers failed: %s", strings.Join(errs, "; "))
}

// QueryServer performs one nonce-bound exchange with a pinned server. It is
// exported for callers that must retain every independently signed response
// and form a quorum rather than accepting the first valid server.
func QueryServer(ctx context.Context, server Server) (Result, error) {
	return queryOne(ctx, server)
}

func queryOne(ctx context.Context, srv Server) (Result, error) {
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Result{}, fmt.Errorf("nonce: %w", err)
	}

	req := encodeFramedRequest(nonce[:], srv.PublicKey)

	conn, err := net.Dial("udp", srv.Address)
	if err != nil {
		return Result{}, fmt.Errorf("dial %s: %w", srv.Address, err)
	}
	defer conn.Close()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(req); err != nil {
		return Result{}, fmt.Errorf("send: %w", err)
	}

	raw := make([]byte, udpBufSize)
	n, err := conn.Read(raw)
	if err != nil {
		return Result{}, fmt.Errorf("recv from %s: %w", srv.Address, err)
	}
	raw = raw[:n]

	return VerifyResponse(raw, nonce[:], srv)
}

// VerifyResponse verifies a raw Roughtime server response against the
// given nonce and server public key. Exported so the agent's multicast
// listener can re-verify a relayed response without re-querying.
func VerifyResponse(rawResp, nonce []byte, srv Server) (Result, error) {
	msg, framed, err := decodeFrame(rawResp)
	if err != nil {
		return Result{}, fmt.Errorf("frame: %w", err)
	}

	outer, err := DecodeMessage(msg)
	if err != nil {
		return Result{}, fmt.Errorf("decode outer: %w", err)
	}

	if framed {
		respNonce, ok := outer[TagNONC]
		if ok && !bytes.Equal(respNonce, nonce) {
			return Result{}, fmt.Errorf("%s: response nonce mismatch", srv.Name)
		}
		ver, ok := outer[TagVER]
		if !ok || len(ver) != 4 || !supportedIETFVersion(binary.LittleEndian.Uint32(ver)) {
			return Result{}, fmt.Errorf("%s: unsupported response version", srv.Name)
		}
	}

	sig, ok := outer[TagSIG]
	if !ok || len(sig) != ed25519.SignatureSize {
		return Result{}, fmt.Errorf("%s: missing or malformed SIG", srv.Name)
	}
	srep, ok := outer[TagSREP]
	if !ok {
		return Result{}, fmt.Errorf("%s: missing SREP", srv.Name)
	}

	verifyKey := srv.PublicKey
	var minTime, maxTime uint64
	if certBytes, ok := outer[TagCERT]; ok {
		verifyKey, minTime, maxTime, err = verifyCertificate(certBytes, srv.PublicKey, srv.Name)
		if err != nil {
			return Result{}, err
		}
	}

	toVerify := append([]byte(SigContext), srep...)
	if !ed25519.Verify(verifyKey, toVerify, sig) {
		return Result{}, fmt.Errorf("%s: Ed25519 response signature invalid", srv.Name)
	}

	inner, err := DecodeMessage(srep)
	if err != nil {
		return Result{}, fmt.Errorf("%s: decode SREP: %w", srv.Name, err)
	}

	midpB, ok := inner[TagMIDP]
	if !ok || len(midpB) != 8 {
		return Result{}, fmt.Errorf("%s: missing MIDP", srv.Name)
	}
	radiB, ok := inner[TagRADI]
	if !ok || len(radiB) != 4 {
		return Result{}, fmt.Errorf("%s: missing RADI", srv.Name)
	}
	root, ok := inner[TagROOT]
	if !ok || len(root) != 32 {
		return Result{}, fmt.Errorf("%s: missing ROOT", srv.Name)
	}

	// Verify the client's nonce is in the Merkle tree.
	indxB := outer[TagINDX] // may be absent for index 0
	path := outer[TagPATH]  // may be absent for a single-client batch
	index := uint32(0)
	if len(indxB) == 4 {
		index = binary.LittleEndian.Uint32(indxB)
	}
	if !verifyMerkle(nonce, root, path, index) {
		return Result{}, fmt.Errorf("%s: nonce not in Merkle tree (possible replay)", srv.Name)
	}

	midpMicros := binary.LittleEndian.Uint64(midpB)
	radiMicros := binary.LittleEndian.Uint32(radiB)
	if maxTime > 0 && (midpMicros < minTime || midpMicros > maxTime) {
		return Result{}, fmt.Errorf("%s: midpoint outside delegation validity", srv.Name)
	}
	if framed {
		midpMicros *= 1_000_000
		radiMicros *= 1_000_000
	}
	if radiMicros > maxRadiusMicros {
		return Result{}, fmt.Errorf("%s: radius %dµs exceeds %dµs limit", srv.Name, radiMicros, maxRadiusMicros)
	}

	return Result{
		Midpoint:    time.UnixMicro(int64(midpMicros)),
		Radius:      time.Duration(radiMicros) * time.Microsecond,
		Server:      srv.Name,
		Nonce:       append([]byte(nil), nonce...),
		RawResponse: rawResp,
	}, nil
}

func encodeFramedRequest(nonce []byte, rootKey ed25519.PublicKey) []byte {
	ver := make([]byte, 8)
	binary.LittleEndian.PutUint32(ver[0:4], VersionDraft11)
	binary.LittleEndian.PutUint32(ver[4:8], VersionDraft08)
	srv := serverID(rootKey)

	msg := EncodeMessage(map[uint32][]byte{
		TagNONC: nonce,
		TagSRV:  srv,
		TagVER:  ver,
	})
	paddingLen := minRequestSize - 12 - len(msg) - 8
	if paddingLen < 0 {
		paddingLen = 0
	}
	if paddingLen%4 != 0 {
		paddingLen += 4 - paddingLen%4
	}
	msg = EncodeMessage(map[uint32][]byte{
		TagNONC: nonce,
		TagSRV:  srv,
		TagVER:  ver,
		TagZZZZ: make([]byte, paddingLen),
	})

	framed := make([]byte, 0, 12+len(msg))
	framed = append(framed, ietfFrameMagic...)
	framed = binary.LittleEndian.AppendUint32(framed, uint32(len(msg)))
	framed = append(framed, msg...)
	return framed
}

func supportedIETFVersion(ver uint32) bool {
	return ver == VersionDraft11 || ver == VersionDraft08
}

func serverID(rootKey ed25519.PublicKey) []byte {
	h := sha512.New()
	h.Write([]byte{0xff}) //nolint:errcheck
	h.Write(rootKey)      //nolint:errcheck
	sum := h.Sum(nil)
	return sum[:32]
}

func decodeFrame(raw []byte) ([]byte, bool, error) {
	if len(raw) < len(ietfFrameMagic) || !bytes.Equal(raw[:len(ietfFrameMagic)], []byte(ietfFrameMagic)) {
		return raw, false, nil
	}
	if len(raw) < 12 {
		return nil, false, fmt.Errorf("truncated IETF frame")
	}
	msgLen := binary.LittleEndian.Uint32(raw[8:12])
	if int(msgLen) != len(raw)-12 {
		return nil, false, fmt.Errorf("IETF frame length %d does not match datagram length %d", msgLen, len(raw)-12)
	}
	return raw[12:], true, nil
}

func verifyCertificate(certBytes []byte, rootKey ed25519.PublicKey, serverName string) (ed25519.PublicKey, uint64, uint64, error) {
	cert, err := DecodeMessage(certBytes)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%s: decode CERT: %w", serverName, err)
	}
	sig, ok := cert[TagSIG]
	if !ok || len(sig) != ed25519.SignatureSize {
		return nil, 0, 0, fmt.Errorf("%s: missing or malformed CERT SIG", serverName)
	}
	dele, ok := cert[TagDELE]
	if !ok {
		return nil, 0, 0, fmt.Errorf("%s: missing DELE", serverName)
	}
	if !ed25519.Verify(rootKey, append([]byte(CertContext), dele...), sig) {
		return nil, 0, 0, fmt.Errorf("%s: delegation signature invalid", serverName)
	}

	delegation, err := DecodeMessage(dele)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%s: decode DELE: %w", serverName, err)
	}
	pub, ok := delegation[TagPUBK]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, 0, 0, fmt.Errorf("%s: missing delegated PUBK", serverName)
	}
	mintB, ok := delegation[TagMINT]
	if !ok || len(mintB) != 8 {
		return nil, 0, 0, fmt.Errorf("%s: missing MINT", serverName)
	}
	maxtB, ok := delegation[TagMAXT]
	if !ok || len(maxtB) != 8 {
		return nil, 0, 0, fmt.Errorf("%s: missing MAXT", serverName)
	}
	minTime := binary.LittleEndian.Uint64(mintB)
	maxTime := binary.LittleEndian.Uint64(maxtB)
	if maxTime < minTime {
		return nil, 0, 0, fmt.Errorf("%s: invalid delegation validity range", serverName)
	}
	return ed25519.PublicKey(pub), minTime, maxTime, nil
}

// ExtractNonceFromResponse returns the nonce included in a raw Roughtime
// response. IETF responses carry NONC at top level; older Google-style test
// responses in this package carried it inside SREP.
func ExtractNonceFromResponse(rawResp []byte) ([]byte, error) {
	msg, _, err := decodeFrame(rawResp)
	if err != nil {
		return nil, err
	}
	outer, err := DecodeMessage(msg)
	if err != nil {
		return nil, fmt.Errorf("decode outer: %w", err)
	}
	if nonce, ok := outer[TagNONC]; ok {
		if len(nonce) != nonceSize {
			return nil, fmt.Errorf("malformed top-level NONC (len %d)", len(nonce))
		}
		return nonce, nil
	}

	srep, ok := outer[TagSREP]
	if !ok {
		return nil, fmt.Errorf("missing SREP")
	}
	inner, err := DecodeMessage(srep)
	if err != nil {
		return nil, fmt.Errorf("decode SREP: %w", err)
	}
	nonce, ok := inner[TagNONC]
	if !ok || len(nonce) != 64 {
		return nil, fmt.Errorf("missing or malformed NONC in SREP (len %d)", len(nonce))
	}
	return nonce, nil
}

// verifyMerkle checks that nonce is a leaf in the Merkle tree with the given root.
// Leaf hash: SHA-512("\x00" || nonce), truncated to the root size.
// Node hash: SHA-512("\x01" || left || right), truncated to the root size.
func verifyMerkle(nonce, root, path []byte, index uint32) bool {
	if len(root) != 32 {
		return false
	}
	h := sha512.Sum512(append([]byte{0x00}, nonce...))
	current := h[:len(root)]

	for i := 0; i+32 <= len(path); i += 32 {
		sibling := path[i : i+32]
		var combined []byte
		if index&1 == 0 {
			combined = append(append([]byte{0x01}, current...), sibling...)
		} else {
			combined = append(append([]byte{0x01}, sibling...), current...)
		}
		h = sha512.Sum512(combined)
		current = h[:len(root)]
		index >>= 1
	}
	return constEqual(current, root)
}

// constEqual is a constant-time byte comparison.
func constEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
