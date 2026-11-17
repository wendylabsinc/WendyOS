package directstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
)

const PromotedFlowExporterLabel = "EXPORTER-wendy-direct-promoted-flow/1"

// PromotedFlowContext adds exact flow identity and fresh per-stream nonces to
// an already locally validated promotion transcript. The attachment snapshot
// includes both sides' reconciled byte ownership. Never hash a peer-supplied
// attachment/open digest in place of that locally retained state.
func PromotedFlowContext(a Attachment, id uint32, open [32]byte, coordinator, participant [16]byte) ([32]byte, error) {
	if a.Mode != byte(Promoted) || a.SessionID == ([32]byte{}) || a.IdentityDigest == ([32]byte{}) || a.SnapshotDigest == ([32]byte{}) || id == 0 || open == ([32]byte{}) || coordinator == ([16]byte{}) || participant == ([16]byte{}) {
		return [32]byte{}, ErrBounds
	}
	base, err := a.Context()
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	h.Write([]byte("wendy/direct/promoted-flow/1\x00"))
	h.Write(base[:])
	var flow [4]byte
	binary.BigEndian.PutUint32(flow[:], id)
	h.Write(flow[:])
	h.Write(open[:])
	h.Write(coordinator[:])
	h.Write(participant[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// BindPromotedFlow authenticates one replacement upper stream for an existing
// authorized flow, never a fresh backend OPEN. Caller has already authenticated
// this candidate's endpoint/role, committed the session promotion transcript,
// and exclusively claimed/frozen this flow. export must be this upper TLS/QUIC
// connection's exporter. The actor elects one initiator per flow; dial direction is independent of logical proof role.
//
// Return commitPossible=true means failure must terminate this flow: its peer
// may have enabled raw bytes. Before that boundary a fresh candidate/nonces may
// retry. Success allows the actor to call RetainedConn.CommitPromotion after
// ensuring old workers are joined. No application bytes are read here.
// Stream ownership stays with caller. ctx bounds this handshake only.
func BindPromotedFlow(ctx context.Context, stream net.Conn, b SessionBinding, a Attachment, id uint32, request OpenRequest, localNonce [16]byte, initiator bool, export Exporter) (commitPossible bool, err error) {
	if stream == nil || export == nil || b.Role > 1 || a.Mode != byte(Promoted) || a.SessionID != b.SessionID || a.IdentityDigest != b.IdentityDigest || b.SessionID == ([32]byte{}) || b.Capability == ([32]byte{}) || b.IdentityDigest == ([32]byte{}) || localNonce == ([16]byte{}) {
		return false, ErrBounds
	}
	open, err := request.digest()
	if err != nil || id == 0 {
		return false, ErrBounds
	}
	if _, err = PromotedFlowContext(a, id, open, localNonce, localNonce); err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer func() {
		if !stop() && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if err = ctx.Err(); err != nil {
		return false, err
	}
	var coordinator, participant [16]byte
	var proof, other [32]byte
	var contextHash [32]byte
	derive := func() error {
		var e error
		contextHash, e = PromotedFlowContext(a, id, open, coordinator, participant)
		if e != nil {
			return e
		}
		exported, e := export(PromotedFlowExporterLabel, contextHash[:], 32)
		if e != nil {
			return e
		}
		if len(exported) != 32 {
			return ErrBounds
		}
		var key [32]byte
		copy(key[:], exported)
		proof, e = AttachmentProof(b.Capability, key, contextHash, b.Role)
		if e != nil {
			return e
		}
		other, e = AttachmentProof(b.Capability, key, contextHash, 1-b.Role)
		return e
	}
	var hello [64]byte
	copy(hello[:4], "WDP1")
	copy(hello[4:36], a.SessionID[:])
	binary.BigEndian.PutUint64(hello[36:44], a.Generation)
	binary.BigEndian.PutUint32(hello[44:48], id)
	if initiator {
		if b.Role == 0 {
			coordinator = localNonce
		} else {
			participant = localNonce
		}
		copy(hello[48:], localNonce[:])
		if err = writeBinding(stream, hello[:]); err != nil {
			return false, err
		}
		var response [48]byte
		if _, err = io.ReadFull(stream, response[:]); err != nil {
			return false, err
		}
		if b.Role == 0 {
			copy(participant[:], response[:16])
		} else {
			copy(coordinator[:], response[:16])
		}
		if err = derive(); err != nil {
			return false, err
		}
		if !hmac.Equal(other[:], response[16:]) {
			return false, ErrAttachmentProof
		}
		commitPossible = true // even a failed Write may have reached the peer
		if err = writeBinding(stream, proof[:]); err != nil {
			return true, err
		}
		var ready [1]byte
		if _, err = io.ReadFull(stream, ready[:]); err != nil {
			return true, err
		}
		if ready[0] != 1 {
			return true, ErrAttachmentProof
		}
		return true, nil
	}
	var incoming [64]byte
	if _, err = io.ReadFull(stream, incoming[:]); err != nil {
		return false, err
	}
	if !bytes.Equal(hello[:48], incoming[:48]) {
		return false, ErrAttachmentProof
	}
	if b.Role == 1 {
		copy(coordinator[:], incoming[48:])
		participant = localNonce
	} else {
		copy(participant[:], incoming[48:])
		coordinator = localNonce
	}
	if err = derive(); err != nil {
		return false, err
	}
	var response [48]byte
	copy(response[:16], localNonce[:])
	copy(response[16:], proof[:])
	if err = writeBinding(stream, response[:]); err != nil {
		return false, err
	}
	var received [32]byte
	if _, err = io.ReadFull(stream, received[:]); err != nil {
		return false, err
	}
	if !hmac.Equal(other[:], received[:]) {
		return false, ErrAttachmentProof
	}
	commitPossible = true
	if err = writeBinding(stream, []byte{1}); err != nil {
		return true, err
	}
	return true, nil
}
func writeBinding(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}
