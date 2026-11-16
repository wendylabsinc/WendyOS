package directstream

import (
	"crypto/sha256"
	"encoding/binary"
)

// RecoveryRecord carries the immutable OPEN identity as well as byte ownership.
// Lists are strictly ascending by ID. Result/closure flags remain present until
// bilateral retirement; a restarted destination is never silently redialled.
// OpenComplete=false is an unresolved OPEN, not an absent/closed application.
type RecoveryRecord struct {
	ID                                             uint32
	OpenDigest                                     [32]byte
	Result, ResetCode                              uint16
	OpenComplete, LocalClosed, RemoteClosed, Reset bool
	Flow                                           FlowSnapshot
}

const RecoveryRecordSize = 113

func (r RecoveryRecord) valid() bool {
	return r.ID != 0 && r.Flow.valid() && (r.Reset || r.ResetCode == 0) && (r.OpenComplete || (r.Result == 0 && !r.LocalClosed && !r.RemoteClosed))
}
func (r RecoveryRecord) Encode(dst []byte) error {
	if len(dst) < RecoveryRecordSize || !r.valid() {
		return ErrBounds
	}
	binary.BigEndian.PutUint32(dst, r.ID)
	copy(dst[4:36], r.OpenDigest[:])
	binary.BigEndian.PutUint16(dst[36:38], r.Result)
	var flags byte
	if r.OpenComplete {
		flags |= 1
	}
	if r.LocalClosed {
		flags |= 2
	}
	if r.RemoteClosed {
		flags |= 4
	}
	if r.Flow.SendFinished {
		flags |= 8
	}
	if r.Flow.ReceiveFinished {
		flags |= 16
	}
	if r.Reset {
		flags |= 32
	}
	dst[38] = flags
	binary.BigEndian.PutUint16(dst[39:41], r.ResetCode)
	values := [9]uint64{r.Flow.Acked, r.Flow.Offered, r.Flow.Tail, r.Flow.RemoteLimit, r.Flow.Consumed, r.Flow.Received, r.Flow.Limit, r.Flow.SendFinal, r.Flow.ReceiveFinal}
	for i, v := range values {
		binary.BigEndian.PutUint64(dst[41+8*i:], v)
	}
	return nil
}
func DecodeRecoveryRecord(src []byte) (RecoveryRecord, error) {
	if len(src) != RecoveryRecordSize || src[38]&^byte(63) != 0 {
		return RecoveryRecord{}, ErrBounds
	}
	r := RecoveryRecord{ID: binary.BigEndian.Uint32(src), Result: binary.BigEndian.Uint16(src[36:]), OpenComplete: src[38]&1 != 0, LocalClosed: src[38]&2 != 0, RemoteClosed: src[38]&4 != 0}
	r.Reset = src[38]&32 != 0
	r.ResetCode = binary.BigEndian.Uint16(src[39:41])
	copy(r.OpenDigest[:], src[4:36])
	v := [9]uint64{}
	for i := range v {
		v[i] = binary.BigEndian.Uint64(src[41+8*i:])
	}
	r.Flow = FlowSnapshot{Acked: v[0], Offered: v[1], Tail: v[2], RemoteLimit: v[3], Consumed: v[4], Received: v[5], Limit: v[6], SendFinal: v[7], ReceiveFinal: v[8], SendFinished: src[38]&8 != 0, ReceiveFinished: src[38]&16 != 0}
	if !r.valid() {
		return RecoveryRecord{}, ErrBounds
	}
	return r, nil
}

// NamespaceCheckpoint records cumulative admission, not a replaceable count of
// live flows. Index 0 belongs to coordinator-initiated IDs, index 1 participant.
// Both endpoint views include their observed/issued credit for both namespaces.
type NamespaceCheckpoint struct {
	HighWater       uint32
	Admitted, Grant uint64
}
type RecoveryView struct {
	Namespaces [2]NamespaceCheckpoint
	Records    []RecoveryRecord
}

func (v RecoveryView) valid() bool {
	if len(v.Records) > 65535 {
		return false
	}
	for i, n := range v.Namespaces {
		if n.Admitted > n.Grant {
			return false
		}
		if n.HighWater == 0 {
			if n.Admitted != 0 {
				return false
			}
		} else {
			if n.HighWater&1 != uint32(i) || n.Admitted == 0 {
				return false
			}
			maximum := uint64(n.HighWater) / 2
			if i == 1 {
				maximum++
			}
			if n.Admitted > maximum {
				return false
			}
		}
	}
	var previous uint32
	var counts [2]uint64
	for _, r := range v.Records {
		if !r.valid() || r.ID <= previous || r.ID > v.Namespaces[r.ID&1].HighWater {
			return false
		}
		previous = r.ID
		counts[r.ID&1]++
		if counts[r.ID&1] > v.Namespaces[r.ID&1].Admitted {
			return false
		}
	}
	return true
}

// RecoveryDigest hashes locally captured/validated views in logical-role order.
// Never use a digest supplied by the peer as a replacement for this computation.
// This fixed encoding is streamed, so no transcript-sized allocation is needed.
func RecoveryDigest(coordinator, participant RecoveryView) ([32]byte, error) {
	if !coordinator.valid() || !participant.valid() {
		return [32]byte{}, ErrBounds
	}
	h := sha256.New()
	h.Write([]byte("wendy/direct/recovery-view/1\x00"))
	var header [42]byte
	var record [RecoveryRecordSize]byte
	for _, v := range []RecoveryView{coordinator, participant} {
		for i, n := range v.Namespaces {
			p := header[i*20:]
			binary.BigEndian.PutUint32(p, n.HighWater)
			binary.BigEndian.PutUint64(p[4:], n.Admitted)
			binary.BigEndian.PutUint64(p[12:], n.Grant)
		}
		binary.BigEndian.PutUint16(header[40:], uint16(len(v.Records)))
		h.Write(header[:])
		for _, r := range v.Records {
			if err := r.Encode(record[:]); err != nil {
				return [32]byte{}, err
			}
			h.Write(record[:])
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
