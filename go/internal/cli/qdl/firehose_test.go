package qdl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ack(attrs ...string) []byte {
	return env(`<response value="ACK" ` + strings.Join(attrs, " ") + `/>`)
}

func nak() []byte { return env(`<response value="NAK"/>`) }

func env(inner string) []byte {
	return []byte(`<?xml version="1.0" ?><data>` + inner + `</data>`)
}

// replyConn models a Firehose device: it answers a request and then stays
// quiet. The silence matters, because await() keeps draining after it has a
// response (logs can trail one), and an unprompted queue would let it swallow
// the reply to the *next* request.
type replyConn struct {
	replies [][]byte
	writes  [][]byte
	armed   bool
}

func (c *replyConn) Read(p []byte, _ time.Duration) (int, error) {
	if !c.armed || len(c.replies) == 0 {
		return 0, ErrTimeout
	}
	c.armed = false
	msg := c.replies[0]
	c.replies = c.replies[1:]
	return copy(p, msg), nil
}

func (c *replyConn) Write(p []byte, _ time.Duration) (int, error) {
	c.writes = append(c.writes, bytes.Clone(p))
	c.armed = true
	return len(p), nil
}

func (c *replyConn) Close() error { return nil }

// session builds a Session over a scripted device and skips negotiation by
// pinning the payload size, so tests can pick a chunk size they can reason about.
func session(t *testing.T, payload int, replies ...[]byte) (*Session, *replyConn, *[]string) {
	t.Helper()
	c := &replyConn{replies: replies}
	var logs []string
	s := NewSession(c, func(l string) { logs = append(logs, l) })
	s.maxPayload = payload
	return s, c, &logs
}

// rawSession drives await() directly, with no request to prompt the device, so
// it uses the ungated transport script.
func rawSession(reads ...[]byte) (*Session, *[]string) {
	var logs []string
	s := NewSession(&fakeConn{reads: reads}, func(l string) { logs = append(logs, l) })
	return s, &logs
}

func TestConfigureAcceptsMatchingPayload(t *testing.T) {
	s, c, _ := session(t, initialPayloadSize,
		ack(fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d"`, initialPayloadSize)))
	if err := s.Configure(StorageUFS); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(c.writes) != 1 {
		t.Fatalf("sent %d configure requests, want 1", len(c.writes))
	}
	req := string(c.writes[0])
	for _, want := range []string{
		`MemoryName="ufs"`,
		fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d"`, initialPayloadSize),
		`ZlpAwareHost="1"`,
		`SkipStorageInit="0"`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("configure request %s\nmissing %s", req, want)
		}
	}
}

func TestConfigureAdoptsDevicePayloadSize(t *testing.T) {
	// The device counter-proposes; we must re-issue configure with its number
	// before moving any bulk data, or every chunk would be the wrong size.
	const proposed = 262144
	s, c, _ := session(t, initialPayloadSize,
		ack(fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d" MaxPayloadSizeToTargetInBytesSupported="%d"`,
			initialPayloadSize, proposed)),
		ack(fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d"`, proposed)),
	)
	if err := s.Configure(StorageUFS); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(c.writes) != 2 {
		t.Fatalf("sent %d configure requests, want a retry", len(c.writes))
	}
	if want := fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d"`, proposed); !strings.Contains(string(c.writes[1]), want) {
		t.Errorf("retry did not adopt the device's size: %s", c.writes[1])
	}
	if s.maxPayload != proposed {
		t.Errorf("session payload = %d, want %d", s.maxPayload, proposed)
	}
}

func TestConfigureRejectsNak(t *testing.T) {
	s, _, _ := session(t, initialPayloadSize, nak())
	if err := s.Configure(StorageUFS); err == nil {
		t.Fatal("want an error on NAK, got nil")
	}
}

func TestProgramStreamsAndPadsFinalSector(t *testing.T) {
	dir := t.TempDir()
	// 5 sectors' worth of data plus 100 bytes: the tail must be zero-padded
	// to a whole sector because the programmer expects exact sector maths.
	const sectorSize = 4096
	body := bytes.Repeat([]byte("A"), 5*sectorSize+100)
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	// 2 sectors per chunk -> 3 writes for 6 sectors.
	s, c, _ := session(t, 2*sectorSize, ack(), ack())
	e := ProgramEntry{
		SectorSize: sectorSize, NumSectors: 100, Partition: 0,
		StartSector: "196614", Filename: "rootfs.img", Label: "rootfsA",
	}
	var lastDone, lastTotal int64
	if err := s.Program(context.Background(), dir, e, func(d, tt int64) { lastDone, lastTotal = d, tt }); err != nil {
		t.Fatalf("Program: %v", err)
	}

	req := string(c.writes[0])
	for _, want := range []string{
		`SECTOR_SIZE_IN_BYTES="4096"`,
		`num_partition_sectors="6"`, // rounded up from the payload, not the partition's 100
		`physical_partition_number="0"`,
		`start_sector="196614"`,
		`filename="rootfs.img"`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("program request %s\nmissing %s", req, want)
		}
	}

	data := c.writes[1:]
	if len(data) != 3 {
		t.Fatalf("sent %d data chunks, want 3", len(data))
	}
	for i, w := range data {
		if len(w) != 2*sectorSize {
			t.Errorf("chunk %d is %d bytes, want a whole %d-sector write", i, len(w), 2)
		}
	}
	// The last chunk holds whatever the earlier full chunks did not, then
	// zero padding out to the sector boundary.
	last := data[2]
	tail := len(body) - 2*(2*sectorSize)
	if !bytes.Equal(last[:tail], bytes.Repeat([]byte("A"), tail)) {
		t.Errorf("final chunk lost the payload tail (%d bytes)", tail)
	}
	if !bytes.Equal(last[tail:], make([]byte, len(last)-tail)) {
		t.Errorf("final chunk was not zero-padded past byte %d", tail)
	}
	if want := int64(6 * sectorSize); lastDone != want || lastTotal != want {
		t.Errorf("progress ended at %d/%d bytes, want %d/%d", lastDone, lastTotal, want, want)
	}
}

func TestProgramHonoursFileSectorOffset(t *testing.T) {
	dir := t.TempDir()
	const sectorSize = 512
	// Two distinct sectors; an offset of 1 must send only the second.
	body := append(bytes.Repeat([]byte("0"), sectorSize), bytes.Repeat([]byte("1"), sectorSize)...)
	if err := os.WriteFile(filepath.Join(dir, "part.img"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	s, c, _ := session(t, 8*sectorSize, ack(), ack())
	e := ProgramEntry{
		SectorSize: sectorSize, NumSectors: 2, FileOffset: 1,
		StartSector: "10", Filename: "part.img", Label: "second",
	}
	if err := s.Program(context.Background(), dir, e, nil); err != nil {
		t.Fatalf("Program: %v", err)
	}

	// Exactly one sector must be written: counting from the file size instead
	// of the remainder after the offset would send a second, zero-filled
	// sector past the end of the region.
	req := string(c.writes[0])
	if !strings.Contains(req, `num_partition_sectors="1"`) {
		t.Errorf("request %s\nshould declare 1 sector, not the whole file", req)
	}
	sent := c.writes[1]
	if len(sent) != sectorSize {
		t.Fatalf("wrote %d bytes, want exactly one %d-byte sector", len(sent), sectorSize)
	}
	if !bytes.Equal(sent, bytes.Repeat([]byte("1"), sectorSize)) {
		t.Errorf("wrote %q..., want the second sector of the payload", sent[:8])
	}
}

func TestProgramRejectsOffsetPastEnd(t *testing.T) {
	dir := t.TempDir()
	const sectorSize = 512
	if err := os.WriteFile(filepath.Join(dir, "part.img"), bytes.Repeat([]byte("x"), sectorSize), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := session(t, 8*sectorSize)
	e := ProgramEntry{SectorSize: sectorSize, FileOffset: 4, StartSector: "1", Filename: "part.img", Label: "x"}
	err := s.Program(context.Background(), dir, e, nil)
	if err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("want an offset-past-end error, got %v", err)
	}
}

func TestProgramRejectsOversizePayload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.img"), bytes.Repeat([]byte("x"), 4096*4), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := session(t, 4096)
	e := ProgramEntry{SectorSize: 4096, NumSectors: 2, StartSector: "1", Filename: "big.img", Label: "small"}
	err := s.Program(context.Background(), dir, e, nil)
	if err == nil || !strings.Contains(err.Error(), "holds only 2") {
		t.Fatalf("want a capacity error, got %v", err)
	}
}

func TestProgramSurfacesRefusal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.img"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := session(t, 4096, nak())
	e := ProgramEntry{SectorSize: 4096, NumSectors: 1, StartSector: "1", Filename: "a.img", Label: "efi"}
	err := s.Program(context.Background(), dir, e, nil)
	if err == nil || !strings.Contains(err.Error(), `refused to program "efi"`) {
		t.Fatalf("want a refusal error, got %v", err)
	}
}

func TestProgramSurfacesCommitFailure(t *testing.T) {
	// Setup ACKs but the commit NAKs: the write appeared to work and only the
	// final acknowledgement reveals it did not.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.img"), bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := session(t, 4096, ack(), nak())
	e := ProgramEntry{SectorSize: 4096, NumSectors: 1, StartSector: "1", Filename: "a.img", Label: "efi"}
	err := s.Program(context.Background(), dir, e, nil)
	if err == nil || !strings.Contains(err.Error(), `programming "efi" failed`) {
		t.Fatalf("want a commit failure, got %v", err)
	}
}

func TestPatchPassesExpressionsVerbatim(t *testing.T) {
	s, c, _ := session(t, initialPayloadSize, ack())
	p := PatchEntry{
		SectorSize: 4096, ByteOffset: 88, Filename: "DISK", SizeInBytes: 4,
		StartSector: "NUM_DISK_SECTORS-1.", Value: "CRC32(NUM_DISK_SECTORS-5.,4096)",
		What: "Update Backup Header with CRC of Partition Array.",
	}
	if err := s.Patch(p); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	req := string(c.writes[0])
	// Rewriting either expression would corrupt the GPT the device writes.
	for _, want := range []string{
		`start_sector="NUM_DISK_SECTORS-1."`,
		`value="CRC32(NUM_DISK_SECTORS-5.,4096)"`,
		`filename="DISK"`,
		`byte_offset="88"`,
		`size_in_bytes="4"`,
	} {
		if !strings.Contains(req, want) {
			t.Errorf("patch request %s\nmissing %s", req, want)
		}
	}
}

func TestResetRequestsDelayedPower(t *testing.T) {
	s, c, _ := session(t, initialPayloadSize, ack())
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	req := string(c.writes[0])
	if !strings.Contains(req, `value="reset"`) || !strings.Contains(req, `DelayInSeconds="10"`) {
		t.Errorf("reset request = %s", req)
	}
}

func TestAwaitSplitsConcatenatedEnvelopes(t *testing.T) {
	// A stream transport can deliver several messages in one read; missing the
	// trailing response would strand the flash waiting for it.
	both := append(env(`<log value="hello from the device"/>`), ack()...)
	s, logs := rawSession(both)
	resp, err := s.await(time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if !resp.ack {
		t.Error("lost the response that followed a log in the same read")
	}
	if len(*logs) != 1 || (*logs)[0] != "hello from the device" {
		t.Errorf("logs = %v, want the device's line", *logs)
	}
}

func TestAwaitReassemblesSplitEnvelope(t *testing.T) {
	whole := ack()
	s, _ := rawSession(whole[:20], whole[20:])
	resp, err := s.await(time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if !resp.ack {
		t.Error("failed to reassemble a response split across two reads")
	}
}

func TestAwaitTimesOutWithoutResponse(t *testing.T) {
	// A silent device must fail, not hang: use a tiny budget so the poll loop
	// exits promptly.
	s, _ := rawSession()
	_, err := s.await(10 * time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout, got nil")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error %q should wrap ErrTimeout", err)
	}
}

func TestAwaitIgnoresUnparseableNoise(t *testing.T) {
	s, logs := rawSession(append([]byte("garbage</data>"), ack()...))
	resp, err := s.await(time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if !resp.ack {
		t.Error("noise before the response should not lose it")
	}
	if len(*logs) == 0 {
		t.Error("unparseable noise should be logged, not silently dropped")
	}
}

func TestRequestsAreSelfClosing(t *testing.T) {
	// The programmer's XML parser rejects the expanded <tag></tag> form that
	// Go's encoding/xml always emits, failing at the end of the document. This
	// was only visible against real hardware, so pin it.
	s, c, _ := session(t, initialPayloadSize, ack())
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	req := string(c.writes[0])
	if !strings.Contains(req, "/>") {
		t.Errorf("request is not self-closing: %s", req)
	}
	if strings.Contains(req, "</power>") {
		t.Errorf("request used the expanded element form: %s", req)
	}
}

func TestRequestAttributesAreEscaped(t *testing.T) {
	// Descriptor values arrive from a downloaded bundle, so a quote in one
	// must not be able to close the attribute and inject another element.
	s, c, _ := session(t, initialPayloadSize, ack())
	err := s.Patch(PatchEntry{
		SectorSize: 4096, Filename: "DISK", StartSector: "1",
		Value: `0"/><power value="reset`, What: "hostile",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := string(c.writes[0])
	if strings.Contains(req, `<power`) {
		t.Errorf("attribute injection succeeded: %s", req)
	}
}

func TestProgramRejectsUnsafePayloadPath(t *testing.T) {
	// Program is reachable with an entry that never went through
	// LoadFlashPlan, and the filename comes from a downloaded descriptor.
	s, _, _ := session(t, initialPayloadSize)
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", "a/../../b"} {
		e := ProgramEntry{SectorSize: 4096, StartSector: "1", Filename: name, Label: "x"}
		err := s.Program(context.Background(), t.TempDir(), e, nil)
		if err == nil || !strings.Contains(err.Error(), "unsafe payload path") {
			t.Errorf("%q: want an unsafe-path error, got %v", name, err)
		}
	}
}

func TestConfigureIgnoresAbsurdDevicePayloadSize(t *testing.T) {
	// The payload size is chosen by the device and drives a make([]byte, n),
	// so an absurd one must not be adopted.
	huge := fmt.Sprintf(`MaxPayloadSizeToTargetInBytes="%d"`, int64(1)<<40)
	s, _, _ := session(t, initialPayloadSize, ack(huge))
	if err := s.Configure(StorageUFS); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if s.maxPayload > maxNegotiablePayload {
		t.Errorf("adopted a payload size of %d bytes, above the %d cap", s.maxPayload, maxNegotiablePayload)
	}
}

func TestSanitiseDeviceText(t *testing.T) {
	// Device log text is printed to a terminal.
	got := sanitiseDeviceText("ok\x1b[31mred\x1b]0;title\x07\r\n\x00 done\ttab")
	for _, bad := range []string{"\x1b", "\x07", "\x00", "\r", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("control byte %q survived: %q", bad, got)
		}
	}
	if !strings.Contains(got, "done\ttab") {
		t.Errorf("stripped legitimate text: %q", got)
	}
}

func TestProgramRejectsZeroSectorSize(t *testing.T) {
	// SectorSize is descriptor-supplied and divides; zero used to panic.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.img"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, _ := session(t, initialPayloadSize)
	err := s.Program(context.Background(), dir, ProgramEntry{StartSector: "1", Filename: "p.img", Label: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no sector size") {
		t.Fatalf("want a sector-size error, got %v", err)
	}
}

func TestAwaitHonoursItsBudgetWhileTheDeviceChatters(t *testing.T) {
	// A device that keeps logging but never answers must not spin forever.
	s, _ := rawSession()
	s.conn = endlessLogs{}
	start := time.Now()
	if _, err := s.await(150 * time.Millisecond); err == nil {
		t.Fatal("want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("await took %v, ignoring its budget", elapsed)
	}
}

func TestAwaitBoundsTheReassemblyBuffer(t *testing.T) {
	// Bytes that never close an envelope must not grow without limit.
	s, _ := rawSession()
	s.conn = endlessBytes{}
	if _, err := s.await(30 * time.Second); err == nil {
		t.Fatal("want an error")
	}
	if len(s.pending) > maxPendingBytes {
		t.Errorf("pending grew to %d bytes, past the %d cap", len(s.pending), maxPendingBytes)
	}
}

type endlessLogs struct{}

func (endlessLogs) Read(p []byte, _ time.Duration) (int, error) {
	return copy(p, env(`<log value="noise"/>`)), nil
}
func (endlessLogs) Write(p []byte, _ time.Duration) (int, error) { return len(p), nil }
func (endlessLogs) Close() error                                 { return nil }

type endlessBytes struct{}

func (endlessBytes) Read(p []byte, _ time.Duration) (int, error) {
	return copy(p, bytes.Repeat([]byte("x"), len(p))), nil
}
func (endlessBytes) Write(p []byte, _ time.Duration) (int, error) { return len(p), nil }
func (endlessBytes) Close() error                                 { return nil }

func TestConfigureDiscardsLeftoverResponses(t *testing.T) {
	// A retried configure can leave a duplicate ACK queued; if it survived,
	// every later await would read the previous exchange's answer.
	s, _, _ := session(t, initialPayloadSize, ack(), ack(), ack())
	if err := s.Configure(StorageUFS); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 0 {
		t.Errorf("pending still holds %d bytes after Configure", len(s.pending))
	}
	// The next request must see its own response, not a leftover.
	if err := s.Patch(PatchEntry{SectorSize: 4096, Filename: "DISK", StartSector: "1", What: "x"}); err != nil {
		t.Errorf("the following request could not read its own response: %v", err)
	}
}
