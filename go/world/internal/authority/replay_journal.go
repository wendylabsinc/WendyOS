package authority

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const journalMagic = "WWREPLAY2\n"

// ReplayStagingSuffix reserves this entire sibling directory for bounded atomic
// journal replacement. Other configured state/diagnostic paths must not use it.
const ReplayStagingSuffix = ".journal-stage"
const journalVersion = 2
const journalHeaderBytes = 68
const replayLogicalLimit = 16 << 20
const journalFrameLimit = replayLogicalLimit + 4096
const journalCompactBytes = 4 << 20
const journalCompactEntries = 4096
const journalFileLimit = int64(len(journalMagic) + journalHeaderBytes + journalFrameLimit + journalCompactBytes + journalFrameLimit)

// Compaction is synchronous under the cache mutex. Its work is bounded by the
// logical state limit. Normal commits append one frame and sync only that file.
type journalFrame struct {
	Version    int           `json:"version"`
	Kind       string        `json:"kind"`
	Sequence   uint64        `json:"sequence"`
	Previous   string        `json:"previous,omitempty"`
	State      *replayState  `json:"state,omitempty"`
	LastUnixMS int64         `json:"last_unix_ms,omitempty"`
	Revision   uint64        `json:"revision,omitempty"`
	Digest     *string       `json:"digest,omitempty"`
	Nonce      *journalNonce `json:"nonce,omitempty"`
}
type journalNonce struct {
	Key     string `json:"key"`
	Expires int64  `json:"expires"`
}

func strictJournalJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing replay JSON")
	}
	// Internal frames use canonical JSON. Reject ambiguous duplicate fields and
	// alternate encodings by requiring exact re-encoding; legacy import does not.
	canonical, err := json.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return errors.New("noncanonical replay frame")
	}
	return nil
}

func encodeJournalFrame(value journalFrame) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > journalFrameLimit {
		return nil, errors.New("replay frame exceeds limit")
	}
	data := make([]byte, journalHeaderBytes+len(payload))
	binary.BigEndian.PutUint32(data[:4], uint32(len(payload)))
	digest := sha256.Sum256(payload)
	copy(data[4:36], digest[:])
	h := sha256.New()
	h.Write([]byte(journalMagic))
	h.Write(data[:36])
	copy(data[36:68], h.Sum(nil))
	copy(data[journalHeaderBytes:], payload)
	return data, nil
}
func journalPayloadHash(frame []byte) (result [32]byte) { copy(result[:], frame[4:36]); return }

func nonceEncodedBytes(key string, expiry int64) int64 {
	b, _ := json.Marshal(key)
	return int64(len(b) + 1 + len(strconv.FormatInt(expiry, 10)))
}
func logicalSize(state replayState, nonceBytes int64) (int64, error) {
	count := len(state.Nonces)
	state.Nonces = map[string]int64{}
	metadata, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	size := int64(len(metadata)) + nonceBytes
	if count > 0 {
		size += int64(count - 1)
	}
	if size > replayLogicalLimit {
		return size, errReplayStateTooLarge
	}
	return size, nil
}
func validateReplayState(state replayState, max int) (int64, error) {
	if state.Version != Version || state.Nonces == nil || len(state.Nonces) > max || state.LastUnixMS < 0 || (state.Revision != 0 && state.Digest == "") || (state.Revision == 0 && state.Digest != "") {
		return 0, errors.New("invalid replay state")
	}
	var size int64
	for key, expiry := range state.Nonces {
		if key == "" || len(key) > 1024 || expiry < 0 {
			return 0, errors.New("invalid persisted nonce")
		}
		size += nonceEncodedBytes(key, expiry)
	}
	_, err := logicalSize(state, size)
	return size, err
}
func (c *ReplayCache) prepareMutation(s replayState) (journalFrame, int64, error) {
	if c.sequence == math.MaxUint64 {
		return journalFrame{}, 0, errors.New("replay journal sequence exhausted")
	}
	frame := journalFrame{Version: journalVersion, Kind: "mutation", Sequence: c.sequence + 1, Previous: hex.EncodeToString(c.previous[:]), LastUnixMS: s.LastUnixMS, Revision: s.Revision}
	if s.Digest != c.state.Digest {
		d := s.Digest
		frame.Digest = &d
	}
	size := c.nonceBytes
	for key, expiry := range c.state.Nonces {
		if expiry <= s.LastUnixMS {
			size -= nonceEncodedBytes(key, expiry)
		}
	}
	for key, expiry := range s.Nonces {
		prior, exists := c.state.Nonces[key]
		if !exists || prior <= s.LastUnixMS {
			if frame.Nonce != nil {
				return journalFrame{}, 0, errors.New("multiple nonce insertions in replay commit")
			}
			frame.Nonce = &journalNonce{key, expiry}
			size += nonceEncodedBytes(key, expiry)
		} else if prior != expiry {
			return journalFrame{}, 0, errors.New("existing nonce changed")
		}
	}
	_, err := logicalSize(s, size)
	return frame, size, err
}

func (c *ReplayCache) sync(file *os.File) error {
	if c.syncFile != nil {
		return c.syncFile(file)
	}
	return file.Sync()
}
func (c *ReplayCache) syncDir() error {
	if c.syncDirectory != nil {
		return c.syncDirectory(filepath.Dir(c.path))
	}
	dir, err := os.Open(filepath.Dir(c.path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closed := dir.Close()
	if err != nil {
		return err
	}
	return closed
}
func (c *ReplayCache) replaceJournal(state replayState, sequence uint64) error {
	frame, err := encodeJournalFrame(journalFrame{Version: journalVersion, Kind: "base", Sequence: sequence, State: &state})
	if err != nil {
		return err
	}
	name := filepath.Join(c.staging, "next")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		os.Remove(name)
		if !installed {
			f.Close()
		}
	}()
	data := append([]byte(journalMagic), frame...)
	if n, writeErr := f.Write(data); writeErr != nil {
		return writeErr
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err = c.sync(f); err != nil {
		return err
	}
	if err = os.Rename(name, c.path); err != nil {
		return err
	}
	if err = c.syncDir(); err != nil {
		c.poisoned = true
		return fmt.Errorf("uncertain replay replacement: %w", err)
	}
	if err = syncReplayDirectory(c.staging); err != nil {
		c.poisoned = true
		return fmt.Errorf("uncertain replay staging cleanup: %w", err)
	}
	old := c.file
	c.file = f
	installed = true
	c.sequence = sequence
	c.previous = journalPayloadHash(frame)
	c.offset = int64(len(data))
	c.appendedBytes = 0
	c.appendedEntries = 0
	if old != nil {
		if err = old.Close(); err != nil {
			c.poisoned = true
			return fmt.Errorf("replay descriptor close: %w", err)
		}
	}
	return nil
}

func (c *ReplayCache) openJournal() error {
	if err := c.ensureStaging(); err != nil {
		return err
	}
	f, err := os.OpenFile(c.path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	c.file = f
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > journalFileLimit {
		return errors.New("invalid replay file type or size")
	}
	prefix := make([]byte, len(journalMagic))
	n, readErr := f.ReadAt(prefix, 0)
	if n != len(prefix) || string(prefix) != journalMagic {
		if info.Size() > replayLogicalLimit {
			return errors.New("legacy replay snapshot exceeds limit")
		}
		data, err := io.ReadAll(io.NewSectionReader(f, 0, info.Size()))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &c.state); err != nil {
			return err
		}
		c.nonceBytes, err = validateReplayState(c.state, c.max)
		if err != nil {
			return err
		}
		return c.replaceJournal(c.state, 0)
	}
	if readErr != nil {
		return readErr
	}
	offset := int64(len(journalMagic))
	first := true
	for offset < info.Size() {
		start := offset
		header := make([]byte, journalHeaderBytes)
		n, err = f.ReadAt(header, offset)
		if errors.Is(err, io.EOF) && n < journalHeaderBytes {
			if first {
				return errors.New("incomplete replay base header")
			}
			return c.recoverTail(start)
		}
		if err != nil {
			return err
		}
		h := sha256.New()
		h.Write([]byte(journalMagic))
		h.Write(header[:36])
		if !bytes.Equal(h.Sum(nil), header[36:]) {
			return errors.New("replay header checksum mismatch")
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		if length <= 0 || length > journalFrameLimit {
			return errors.New("invalid replay frame length")
		}
		if info.Size()-offset-journalHeaderBytes < length {
			if first {
				return errors.New("incomplete replay base")
			}
			return c.recoverTail(start)
		}
		payload := make([]byte, int(length))
		if _, err = f.ReadAt(payload, offset+journalHeaderBytes); err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		if !bytes.Equal(digest[:], header[4:36]) {
			return errors.New("replay payload checksum mismatch")
		}
		var frame journalFrame
		if err = strictJournalJSON(payload, &frame); err != nil {
			return err
		}
		if first {
			if frame.Version != journalVersion || frame.Kind != "base" || frame.State == nil || frame.Previous != "" || frame.LastUnixMS != 0 || frame.Revision != 0 || frame.Digest != nil || frame.Nonce != nil {
				return errors.New("invalid replay base")
			}
			c.nonceBytes, err = validateReplayState(*frame.State, c.max)
			if err != nil {
				return err
			}
			c.state = *frame.State
			c.sequence = frame.Sequence
			first = false
		} else {
			if err = c.replayMutation(frame); err != nil {
				return err
			}
			c.appendedBytes += journalHeaderBytes + length
			c.appendedEntries++
			if c.appendedBytes > journalCompactBytes || c.appendedEntries > journalCompactEntries {
				return errors.New("replay journal exceeded compaction bound")
			}
		}
		c.previous = digest
		offset += journalHeaderBytes + length
		c.offset = offset
	}
	if first {
		return errors.New("replay journal has no base")
	}
	return nil
}

func (c *ReplayCache) recoverTail(offset int64) error {
	// Only an incomplete final frame with no conflicting complete checksum is
	// recoverable. External truncation of an acknowledged prefix is outside the
	// trusted local filesystem model and cannot be distinguished from a crash.
	if err := c.file.Truncate(offset); err != nil {
		return err
	}
	if err := c.sync(c.file); err != nil {
		c.poisoned = true
		return err
	}
	c.offset = offset
	return nil
}
func (c *ReplayCache) replayMutation(frame journalFrame) error {
	if frame.Version != journalVersion || frame.Kind != "mutation" || frame.State != nil || c.sequence == math.MaxUint64 || frame.Sequence != c.sequence+1 || frame.Previous != hex.EncodeToString(c.previous[:]) || frame.LastUnixMS <= 0 || frame.LastUnixMS < c.state.LastUnixMS || frame.Revision < c.state.Revision {
		return errors.New("invalid replay mutation order")
	}
	s := c.state
	s.LastUnixMS = frame.LastUnixMS
	s.Revision = frame.Revision
	if frame.Digest != nil {
		s.Digest = *frame.Digest
	}
	if (s.Revision == c.state.Revision && s.Digest != c.state.Digest) || (s.Revision > 0 && s.Digest == "") || (s.Revision == 0 && s.Digest != "") {
		return errors.New("invalid replay revision digest")
	}
	s.Nonces = make(map[string]int64, len(c.state.Nonces))
	size := c.nonceBytes
	for key, expiry := range c.state.Nonces {
		if expiry > s.LastUnixMS {
			s.Nonces[key] = expiry
		} else {
			size -= nonceEncodedBytes(key, expiry)
		}
	}
	if nonce := frame.Nonce; nonce != nil {
		if nonce.Key == "" || len(nonce.Key) > 1024 || nonce.Expires < s.LastUnixMS || len(s.Nonces) >= c.max {
			return errors.New("invalid journal nonce")
		}
		if _, exists := s.Nonces[nonce.Key]; exists {
			return errors.New("duplicate journal nonce")
		}
		s.Nonces[nonce.Key] = nonce.Expires
		size += nonceEncodedBytes(nonce.Key, nonce.Expires)
	}
	if _, err := logicalSize(s, size); err != nil {
		return err
	}
	c.state = s
	c.nonceBytes = size
	c.sequence = frame.Sequence
	return nil
}
