package services

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const gdpTagBaseForTests = 3_600_000_000_000_000

// What the feasibility spike's writer sent to gdpdepay for one four-byte frame
// tagged 1000 hours: stream-start, caps, segment, buffer.
const gdpSpikeWriterStream = "" +
	"01000000284e00000074ffffffffffffffff0000000000000000000000000000000000000000000000000000000000000000" +
	"0000000000000000000000004773744576656e7453747265616d53746172742c2073747265616d2d69643d28737472696e67" +
	"2977656e64792d646572697665642c20666c6167733d2847737453747265616d466c616773294753545f53545245414d5f46" +
	"4c41475f4e4f4e452c2067726f75702d69643d2875696e7429313b0001000000000200000046000000000000000000000000" +
	"00000000000000000000000000000000000000000000000000000000000000000000000000000000696d6167652f6a706567" +
	"2c2077696474683d28696e74293634302c206865696768743d28696e74293438302c206672616d65726174653d2866726163" +
	"74696f6e2933302f310001000000464e00000147ffffffffffffffff00000000000000000000000000000000000000000000" +
	"000000000000000000000000000000000000000000004773744576656e745365676d656e742c207365676d656e743d284773" +
	"745365676d656e7429227365676d656e742c20666c6167733d284773745365676d656e74466c616773294753545f5345474d" +
	"454e545f464c41475f4e4f4e452c20726174653d28646f75626c6529312c206170706c6965642d726174653d28646f75626c" +
	"6529312c20666f726d61743d28477374466f726d61742974696d652c20626173653d286775696e74363429302c206f666673" +
	"65743d286775696e74363429302c2073746172743d286775696e74363429302c2073746f703d286775696e74363429313834" +
	"34363734343037333730393535313631352c2074696d653d286775696e74363429302c20706f736974696f6e3d286775696e" +
	"74363429302c206475726174696f6e3d286775696e7436342931383434363734343037333730393535313631353b223b0001" +
	"000000000100000004000cca2e51310000ffffffffffffffffffffffffffffffffffffffffffffffff0000000cca2e513100" +
	"00000000000000000000004a504547"

// What gdppay 1.28 wrote for two 2x2 GRAY8 pictures: stream-start, caps,
// segment, then buffers stamped 0 and 33333333 ns, with header checksums.
const gdpPayCapture = "" +
	"01000100284e00000087ffffffffffffffff0000000000000000000000000000000000000000000000000000000000000000" +
	"0000000000000000fe1f00004773744576656e7453747265616d53746172742c2073747265616d2d69643d28737472696e67" +
	"2934353464326237326438366462343866323134616433353664666264373863302c20666c6167733d284773745374726561" +
	"6d466c616773294753545f53545245414d5f464c41475f4e4f4e452c2067726f75702d69643d2875696e7429313b00010001" +
	"000002000000bc00000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
	"00000000000ed00000766964656f2f782d7261772c20666f726d61743d28737472696e672947524159382c2077696474683d" +
	"28696e7429322c206865696768743d28696e7429322c206672616d65726174653d286672616374696f6e2933302f312c206d" +
	"756c7469766965772d6d6f64653d28737472696e67296d6f6e6f2c20706978656c2d6173706563742d726174696f3d286672" +
	"616374696f6e29312f312c20696e7465726c6163652d6d6f64653d28737472696e672970726f677265737369766500010001" +
	"00464e00000147ffffffffffffffff0000000000000000000000000000000000000000000000000000000000000000000000" +
	"000000000050a500004773744576656e745365676d656e742c207365676d656e743d284773745365676d656e742922736567" +
	"6d656e742c20666c6167733d284773745365676d656e74466c616773294753545f5345474d454e545f464c41475f4e4f4e45" +
	"2c20726174653d28646f75626c6529312c206170706c6965642d726174653d28646f75626c6529312c20666f726d61743d28" +
	"477374466f726d61742974696d652c20626173653d286775696e74363429302c206f66667365743d286775696e7436342930" +
	"2c2073746172743d286775696e74363429302c2073746f703d286775696e7436342931383434363734343037333730393535" +
	"313631352c2074696d653d286775696e74363429302c20706f736974696f6e3d286775696e74363429302c20647572617469" +
	"6f6e3d286775696e7436342931383434363734343037333730393535313631353b223b000100010000010000000800000000" +
	"000000000000000001fca055000000000000000000000000000000010040ffffffffffffffff000000000000ce6c00009129" +
	"000010100000010001000001000000080000000001fca0550000000001fca055000000000000000100000000000000020000" +
	"ffffffffffffffff000000000000b65e00009129000010cd0000"

// The segment event a hardware H.264 encoder's gdppay wrote after it moved a
// stream whose first timestamp was zero up by 1000 hours.
const gdpShiftedSegmentPacket = "" +
	"01000100464e00000165ffffffffffffffff0000000000000000000000000000000000000000000000000000000000000000" +
	"0000000000000000f41a00004773744576656e745365676d656e742c207365676d656e743d284773745365676d656e742922" +
	"7365676d656e742c20666c6167733d284773745365676d656e74466c616773294753545f5345474d454e545f464c41475f4e" +
	"4f4e452c20726174653d28646f75626c6529312c206170706c6965642d726174653d28646f75626c6529312c20666f726d61" +
	"743d28477374466f726d61742974696d652c20626173653d286775696e74363429302c206f66667365743d286775696e7436" +
	"3429302c2073746172743d286775696e74363429333630303030303030303030303030302c2073746f703d286775696e7436" +
	"342931383434363734343037333730393535313631352c2074696d653d286775696e74363429302c20706f736974696f6e3d" +
	"286775696e74363429333630303030303030303030303030302c206475726174696f6e3d286775696e743634293138343436" +
	"3734343037333730393535313631353b223b00"

func gdpFixture(t *testing.T, hexStream string) []byte {
	t.Helper()
	b, err := hex.DecodeString(hexStream)
	if err != nil {
		t.Fatalf("fixture is not hex: %v", err)
	}
	return b
}

func TestGDPWriterMatchesTheBytesTheSpikeFedGdpdepay(t *testing.T) {
	var out bytes.Buffer
	w := &gdpWriter{w: &out}
	if err := w.streamHeader("image/jpeg, width=(int)640, height=(int)480, framerate=(fraction)30/1"); err != nil {
		t.Fatal(err)
	}
	if err := w.buffer(gdpTagBaseForTests, []byte("JPEG")); err != nil {
		t.Fatal(err)
	}
	if want := gdpFixture(t, gdpSpikeWriterStream); !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("writer produced\n%x\nwant\n%x", out.Bytes(), want)
	}
}

func TestGDPWriterReportsAFailedWrite(t *testing.T) {
	w := &gdpWriter{w: failingWriter{}}
	if err := w.buffer(1, []byte("JPEG")); err == nil {
		t.Fatal("a write the pipe refused was reported as sent")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGDPReaderReadsWhatGdppayWrote(t *testing.T) {
	r := &gdpReader{r: bytes.NewReader(gdpFixture(t, gdpPayCapture)), maxPayload: 64}
	for i, want := range []struct {
		timestamp uint64
		payload   string
	}{
		{0, "9129000010100000"},
		{33333333, "9129000010cd0000"},
	} {
		timestamp, payload, err := r.nextBuffer()
		if err != nil {
			t.Fatalf("buffer %d: %v", i, err)
		}
		if timestamp != want.timestamp || hex.EncodeToString(payload) != want.payload {
			t.Errorf("buffer %d = %d %x, want %d %s", i, timestamp, payload, want.timestamp, want.payload)
		}
	}
	if r.segmentStart != 0 {
		t.Errorf("segment start = %d, want 0", r.segmentStart)
	}
	if _, _, err := r.nextBuffer(); err != io.EOF {
		t.Fatalf("after the last packet err = %v, want io.EOF", err)
	}
}

func TestGDPReaderLearnsTheSegmentStartAnEncoderMovedTheStreamTo(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(gdpFixture(t, gdpShiftedSegmentPacket))
	w := &gdpWriter{w: &stream}
	if err := w.buffer(gdpTagBaseForTests+5, []byte("unit")); err != nil {
		t.Fatal(err)
	}
	r := &gdpReader{r: &stream, maxPayload: 64}
	timestamp, _, err := r.nextBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if r.segmentStart != gdpTagBaseForTests {
		t.Fatalf("segment start = %d, want %d", r.segmentStart, uint64(gdpTagBaseForTests))
	}
	if got := timestamp - r.segmentStart; got != 5 {
		t.Errorf("timestamp less the segment start = %d, want 5", got)
	}
}

func TestGDPRoundTripKeepsEveryTimestampAndPayload(t *testing.T) {
	var stream bytes.Buffer
	w := &gdpWriter{w: &stream}
	if err := w.streamHeader("image/jpeg"); err != nil {
		t.Fatal(err)
	}
	payloads := [][]byte{[]byte("first"), {}, bytes.Repeat([]byte{0xAB}, 4096)}
	for i, p := range payloads {
		if err := w.buffer(uint64(1000+i), p); err != nil {
			t.Fatal(err)
		}
	}

	r := &gdpReader{r: bytes.NewReader(stream.Bytes()), maxPayload: 8192}
	var kinds []uint16
	for {
		packet, err := r.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, packet.kind)
	}
	want := []uint16{gdpPacketStreamStart, gdpPacketCaps, gdpPacketSegment, gdpPacketBuffer, gdpPacketBuffer, gdpPacketBuffer}
	if len(kinds) != len(want) {
		t.Fatalf("read packet kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("read packet kinds %v, want %v", kinds, want)
		}
	}

	r = &gdpReader{r: bytes.NewReader(stream.Bytes()), maxPayload: 8192}
	for i, p := range payloads {
		timestamp, payload, err := r.nextBuffer()
		if err != nil {
			t.Fatalf("buffer %d: %v", i, err)
		}
		if timestamp != uint64(1000+i) || !bytes.Equal(payload, p) {
			t.Errorf("buffer %d = %d (%d bytes), want %d (%d bytes)", i, timestamp, len(payload), 1000+i, len(p))
		}
	}
}

func TestGDPReaderAssemblesPacketsFromShortReads(t *testing.T) {
	r := &gdpReader{r: iotest.OneByteReader(bytes.NewReader(gdpFixture(t, gdpPayCapture))), maxPayload: 64}
	var timestamps []uint64
	for {
		timestamp, _, err := r.nextBuffer()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		timestamps = append(timestamps, timestamp)
	}
	if len(timestamps) != 2 || timestamps[0] != 0 || timestamps[1] != 33333333 {
		t.Fatalf("timestamps = %v, want [0 33333333]", timestamps)
	}
}

func TestGDPReaderTellsAStreamCutInsideAPacketFromOneThatEnded(t *testing.T) {
	whole := gdpFixture(t, gdpPayCapture)
	lastBuffer := len(whole) - (gdpHeaderBytes + 8)
	for name, cut := range map[string]int{
		"inside a header":  lastBuffer + 10,
		"inside a payload": len(whole) - 3,
	} {
		t.Run(name, func(t *testing.T) {
			r := &gdpReader{r: bytes.NewReader(whole[:cut]), maxPayload: 64}
			if _, _, err := r.nextBuffer(); err != nil {
				t.Fatalf("first buffer: %v", err)
			}
			if _, _, err := r.nextBuffer(); err != io.ErrUnexpectedEOF {
				t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestGDPReaderSkipsAnOversizedPayloadAndStaysAligned(t *testing.T) {
	var stream bytes.Buffer
	w := &gdpWriter{w: &stream}
	for i, size := range []int{4, 100, 4} {
		if err := w.buffer(uint64(i), bytes.Repeat([]byte{byte(i)}, size)); err != nil {
			t.Fatal(err)
		}
	}
	r := &gdpReader{r: &stream, maxPayload: 16}
	if timestamp, _, err := r.nextBuffer(); err != nil || timestamp != 0 {
		t.Fatalf("first buffer = %d, %v", timestamp, err)
	}
	if _, _, err := r.nextBuffer(); !errors.Is(err, errGDPPayloadTooLarge) {
		t.Fatalf("err = %v, want errGDPPayloadTooLarge", err)
	}
	if timestamp, payload, err := r.nextBuffer(); err != nil || timestamp != 2 || !bytes.Equal(payload, []byte{2, 2, 2, 2}) {
		t.Fatalf("buffer after the oversized one = %d %x, %v", timestamp, payload, err)
	}
}

func TestGDPReaderRefusesBytesThatAreNotAPacket(t *testing.T) {
	whole := gdpFixture(t, gdpPayCapture)
	for name, damage := range map[string]func([]byte){
		"another protocol version":      func(b []byte) { b[0] = 2 },
		"a header failing its checksum": func(b []byte) { b[7] ^= 0x01 },
	} {
		t.Run(name, func(t *testing.T) {
			stream := append([]byte(nil), whole...)
			damage(stream)
			r := &gdpReader{r: bytes.NewReader(stream), maxPayload: 64}
			if _, err := r.next(); !errors.Is(err, errGDPNotAPacket) {
				t.Fatalf("err = %v, want errGDPNotAPacket", err)
			}
		})
	}
}

func TestGDPSegmentStart(t *testing.T) {
	for payload, want := range map[string]uint64{
		`GstEventSegment, segment=(GstSegment)"segment, base=(guint64)0, start=(guint64)3600000000000000, stop=(guint64)1;";`: 3_600_000_000_000_000,
		`GstEventSegment, segment=(GstSegment)"segment, start=(guint64)0;";`:                                                  0,
	} {
		if got, ok := gdpSegmentStart([]byte(payload)); !ok || got != want {
			t.Errorf("gdpSegmentStart(%q) = %d, %v, want %d", payload, got, ok, want)
		}
	}
	for _, payload := range []string{"", "GstEventSegment;", `segment, start=(guint64)later;`, strings.Repeat("x", 300)} {
		if _, ok := gdpSegmentStart([]byte(payload)); ok {
			t.Errorf("gdpSegmentStart(%q) found a start", payload)
		}
	}
}
