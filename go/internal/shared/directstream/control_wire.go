package directstream

import "encoding/binary"

// Session control shares the ordered direct record lane with flow records, but
// has disjoint kinds. The enclosing authenticated connection supplies identity,
// role and an incarnation token. Decoding a record never grants attachment.
const (
	ControlOffer byte = 16 + iota
	ControlView
	ControlRecords
	ControlReady
	ControlCommit
	ControlCommitAck
)
const MaxRecoveryBatch = 4

// ControlOffer starts an initial or replacement attachment. Only a new direct
// BLE session sends Capability; recovery proves possession without sending it.
// The coordinator sends Offer then ordered Records; the participant sends View,
// Records and Ready. Commit carries the coordinator proof, ACK its context.
// Thus an empty session takes four flights, not a separate nonce round trip.
// Each view's Records start at zero, are contiguous and total exactly Count.
// Actors must bound Count by reserved slots before accepting a view, validate
// the complete view, and use the locally computed digest in the transaction.
type ControlRecord struct {
	Kind                  byte
	AttachmentID          uint32
	Generation            uint64
	Mode                  Mode
	Create                bool
	SessionID, Capability [32]byte
	Nonce                 [16]byte
	Namespaces            [2]NamespaceCheckpoint
	Count                 uint16
	Start                 uint16
	Records               []RecoveryRecord
	Context, Proof        [32]byte
}

func (c ControlRecord) Size() (int, error) {
	if c.AttachmentID == 0 {
		return 0, ErrBounds
	}
	switch c.Kind {
	case ControlOffer:
		if c.Generation == 0 || c.Mode > Promoted || c.SessionID == ([32]byte{}) || c.Nonce == ([16]byte{}) || (c.Create && (c.Mode != DirectBLE || c.Capability == ([32]byte{}))) || (!c.Create && c.Capability != ([32]byte{})) {
			return 0, ErrBounds
		}
		return 137, nil
	case ControlView:
		if c.Nonce == ([16]byte{}) {
			return 0, ErrBounds
		}
		return 63, nil
	case ControlRecords:
		if len(c.Records) == 0 || len(c.Records) > MaxRecoveryBatch || int(c.Start)+len(c.Records) > 65535 {
			return 0, ErrBounds
		}
		var previous uint32
		for _, r := range c.Records {
			if !r.valid() || r.ID <= previous {
				return 0, ErrBounds
			}
			previous = r.ID
		}
		return 8 + len(c.Records)*RecoveryRecordSize, nil
	case ControlReady, ControlCommit:
		return 69, nil
	case ControlCommitAck:
		return 37, nil
	default:
		return 0, ErrBounds
	}
}
func putView(dst []byte, c ControlRecord) {
	for i, n := range c.Namespaces {
		p := dst[i*20:]
		binary.BigEndian.PutUint32(p, n.HighWater)
		binary.BigEndian.PutUint64(p[4:], n.Admitted)
		binary.BigEndian.PutUint64(p[12:], n.Grant)
	}
	binary.BigEndian.PutUint16(dst[40:], c.Count)
}
func getView(src []byte, c *ControlRecord) {
	for i := range c.Namespaces {
		p := src[i*20:]
		c.Namespaces[i] = NamespaceCheckpoint{binary.BigEndian.Uint32(p), binary.BigEndian.Uint64(p[4:]), binary.BigEndian.Uint64(p[12:])}
	}
	c.Count = binary.BigEndian.Uint16(src[40:])
}

// Encode writes nothing on invalid input or insufficient capacity.
func (c ControlRecord) Encode(dst []byte) (int, error) {
	n, err := c.Size()
	if err != nil || len(dst) < n {
		return 0, ErrBounds
	}
	dst[0] = c.Kind
	binary.BigEndian.PutUint32(dst[1:], c.AttachmentID)
	switch c.Kind {
	case ControlOffer:
		binary.BigEndian.PutUint64(dst[5:], c.Generation)
		dst[13] = byte(c.Mode)
		dst[14] = 0
		if c.Create {
			dst[14] = 1
		}
		copy(dst[15:47], c.SessionID[:])
		copy(dst[47:79], c.Capability[:])
		copy(dst[79:95], c.Nonce[:])
		putView(dst[95:], c)
	case ControlView:
		copy(dst[5:21], c.Nonce[:])
		putView(dst[21:], c)
	case ControlRecords:
		binary.BigEndian.PutUint16(dst[5:], c.Start)
		dst[7] = byte(len(c.Records))
		for i, r := range c.Records {
			_ = r.Encode(dst[8+i*RecoveryRecordSize:])
		}
	case ControlReady, ControlCommit:
		copy(dst[5:37], c.Context[:])
		copy(dst[37:69], c.Proof[:])
	case ControlCommitAck:
		copy(dst[5:37], c.Context[:])
	}
	return n, nil
}

// DecodeControlRecord bounds allocation to four recovery records. Snapshot
// assembly/storage belongs to the session actor, never a wire-supplied count.
func DecodeControlRecord(src []byte) (ControlRecord, error) {
	var c ControlRecord
	if len(src) < 5 || len(src) > MaxFlowFrame {
		return c, ErrBounds
	}
	c.Kind = src[0]
	c.AttachmentID = binary.BigEndian.Uint32(src[1:])
	switch c.Kind {
	case ControlOffer:
		if len(src) != 137 || src[14] > 1 {
			return ControlRecord{}, ErrBounds
		}
		c.Generation = binary.BigEndian.Uint64(src[5:])
		c.Mode = Mode(src[13])
		c.Create = src[14] == 1
		copy(c.SessionID[:], src[15:47])
		copy(c.Capability[:], src[47:79])
		copy(c.Nonce[:], src[79:95])
		getView(src[95:], &c)
	case ControlView:
		if len(src) != 63 {
			return ControlRecord{}, ErrBounds
		}
		copy(c.Nonce[:], src[5:21])
		getView(src[21:], &c)
	case ControlRecords:
		if len(src) < 8 || src[7] == 0 || src[7] > MaxRecoveryBatch || len(src) != 8+int(src[7])*RecoveryRecordSize {
			return ControlRecord{}, ErrBounds
		}
		c.Start = binary.BigEndian.Uint16(src[5:])
		c.Records = make([]RecoveryRecord, int(src[7]))
		for i := range c.Records {
			r, err := DecodeRecoveryRecord(src[8+i*RecoveryRecordSize : 8+(i+1)*RecoveryRecordSize])
			if err != nil {
				return ControlRecord{}, err
			}
			c.Records[i] = r
		}
	case ControlReady, ControlCommit:
		if len(src) != 69 {
			return ControlRecord{}, ErrBounds
		}
		copy(c.Context[:], src[5:37])
		copy(c.Proof[:], src[37:69])
	case ControlCommitAck:
		if len(src) != 37 {
			return ControlRecord{}, ErrBounds
		}
		copy(c.Context[:], src[5:37])
	default:
		return ControlRecord{}, ErrBounds
	}
	if _, err := c.Size(); err != nil {
		return ControlRecord{}, err
	}
	return c, nil
}
