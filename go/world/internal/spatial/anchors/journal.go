package anchors

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
)

// journalFile is the one file a Store keeps in its directory. A record is a
// big-endian uint32 length, the CRC-32C of the body, and the body: one entry
// as JSON.
const journalFile = "anchors.journal"

// maxEntryBytes bounds one record; an anchor encodes to a few kilobytes.
const maxEntryBytes = 1 << 20

const (
	opPut    = "put"    // the whole anchor: created, or replaced by Update
	opPose   = "pose"   // id, position, yaw and revision after a map correction
	opDelete = "delete" // id only
)

// entry is one journal record. A pose entry leaves the signature and the
// bindings out, so that a map correction costs a hundred bytes per anchor
// rather than the whole record.
type entry struct {
	Op     string         `json:"op"`
	Anchor spatial.Anchor `json:"anchor"`
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func encodeEntries(entries []entry) ([]byte, error) {
	var out []byte
	for _, e := range entries {
		body, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if len(body) > maxEntryBytes {
			return nil, fmt.Errorf("anchors: journal entry of %d bytes", len(body))
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
		out = binary.BigEndian.AppendUint32(out, crc32.Checksum(body, castagnoli))
		out = append(out, body...)
	}
	return out, nil
}

// readJournal decodes every intact record and returns the length of the
// intact prefix. A record that is cut short, a damaged record that is the
// last thing in the file and a tail of zero bytes are a torn tail from an
// interrupted write and are left out; damage with data behind it is
// ErrCorrupt.
func readJournal(f *os.File) ([]entry, int64, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	var entries []entry
	off := 0
	for len(data)-off >= 8 && len(bytes.TrimLeft(data[off:], "\x00")) > 0 {
		n := int(binary.BigEndian.Uint32(data[off:]))
		if n > maxEntryBytes {
			return nil, 0, fmt.Errorf("%w: record length at byte %d", ErrCorrupt, off)
		}
		end := off + 8 + n
		if end > len(data) {
			break
		}
		body := data[off+8 : end]
		var e entry
		if n == 0 || crc32.Checksum(body, castagnoli) != binary.BigEndian.Uint32(data[off+4:]) || json.Unmarshal(body, &e) != nil {
			if end < len(data) {
				return nil, 0, fmt.Errorf("%w: record at byte %d", ErrCorrupt, off)
			}
			break
		}
		entries = append(entries, e)
		off = end
	}
	return entries, int64(off), nil
}
