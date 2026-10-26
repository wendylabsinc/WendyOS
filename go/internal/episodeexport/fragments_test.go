package episodeexport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assembleTestChunks(t *testing.T, payloads [][]byte) (units [][]byte, stamps []int64, fragments, skipped int) {
	t.Helper()
	a := h264Assembler{
		emit: func(payload []byte, timestamp int64) error {
			units = append(units, bytes.Clone(payload))
			stamps = append(stamps, timestamp)
			return nil
		},
		skip: func(cameraIndexLine) { skipped++ },
	}
	for i, payload := range payloads {
		if err := a.add(cameraIndexLine{CanonicalEpisodeNanos: int64(i) * 1000}, i+1, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.flush(); err != nil {
		t.Fatal(err)
	}
	return units, stamps, a.fragments, skipped
}

func TestH264AssemblerContinuationAndMixedRead(t *testing.T) {
	key := annexB([]byte{0x65, 0xB8, 0xAA, 0xBB, 0xCC})
	inter := annexB([]byte{0x41, 0xC0, 0xDD})
	for _, tc := range []struct {
		name      string
		chunks    [][]byte
		fragments int
		second    int64
	}{
		{"continuations", [][]byte{key[:7], key[7:8], key[8:], inter}, 2, 3000},
		{"tail and next picture", [][]byte{key[:7], append(bytes.Clone(key[7:]), inter...)}, 1, 1000},
		{"two pictures in one read", [][]byte{append(bytes.Clone(key), inter...)}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units, stamps, fragments, skipped := assembleTestChunks(t, tc.chunks)
			if len(units) != 2 || !bytes.Equal(units[0], key) || !bytes.Equal(units[1], inter) {
				t.Fatalf("units = %x; want %x, %x", units, key, inter)
			}
			if stamps[0] != 0 || stamps[1] != tc.second || fragments != tc.fragments || skipped != 0 {
				t.Fatalf("stamps=%v fragments=%d skipped=%d", stamps, fragments, skipped)
			}
		})
	}
}

func TestH264AssemblerMultiSliceAndLeadingNALs(t *testing.T) {
	firstSlice := annexB([]byte{0x65, 0xB8, 0xAA})
	// first_mb_in_slice = 1 (010), slice_type = 2 (011).
	secondSlice := annexB([]byte{0x65, 0x4C, 0xBB})
	inter := annexB([]byte{0x41, 0xC0, 0xCC})
	for _, leading := range [][]byte{
		nil, annexB([]byte{9, 0xF0}), annexB([]byte{6, 0x80}),
		annexB([]byte{7, 0x42}, []byte{8, 0x80}),
	} {
		t.Run(fmt.Sprintf("leading_bytes_%d", len(leading)), func(t *testing.T) {
			key := append(bytes.Clone(firstSlice), secondSlice...)
			next := append(bytes.Clone(leading), inter...)
			// The first continuation carries another slice of the same picture;
			// the second carries the tail of that slice and the next picture.
			chunks := [][]byte{firstSlice[:6], append(bytes.Clone(firstSlice[6:]), secondSlice[:6]...), append(bytes.Clone(secondSlice[6:]), next...)}
			units, stamps, fragments, skipped := assembleTestChunks(t, chunks)
			if len(units) != 2 || !bytes.Equal(units[0], key) || !bytes.Equal(units[1], next) {
				t.Fatalf("units = %x; want %x, %x", units, key, next)
			}
			if stamps[1] != 2000 || fragments != 2 || skipped != 0 {
				t.Fatalf("stamps=%v fragments=%d skipped=%d", stamps, fragments, skipped)
			}
		})
	}
}

func TestH264AssemblerEverySplitPreservesBytesAndFirstChunkTime(t *testing.T) {
	key := annexB([]byte{7, 0x42}, []byte{8, 0x80}, []byte{0x65, 0xB8, 0xAA})
	for _, prefix := range [][]byte{{0, 0, 1}, {0, 0, 0, 1}} {
		inter := append(bytes.Clone(prefix), 0x41, 0xC0, 0xBB)
		stream := append(bytes.Clone(key), inter...)
		// Includes splits inside the prefix, between prefix and header, and
		// between NAL header and first_mb_in_slice.
		for cut := 1; cut < len(stream); cut++ {
			units, stamps, _, skipped := assembleTestChunks(t, [][]byte{stream[:cut], stream[cut:]})
			if len(units) != 2 || !bytes.Equal(units[0], key) || !bytes.Equal(units[1], inter) {
				t.Fatalf("prefix=%d cut=%d units=%x", len(prefix), cut, units)
			}
			wantSecond := int64(0)
			if cut <= len(key) {
				wantSecond = 1000
			}
			if stamps[0] != 0 || stamps[1] != wantSecond || skipped != 0 {
				t.Fatalf("cut=%d stamps=%v skipped=%d", cut, stamps, skipped)
			}
		}
		var chunks [][]byte
		for _, b := range stream {
			chunks = append(chunks, []byte{b})
		}
		units, stamps, _, skipped := assembleTestChunks(t, chunks)
		if len(units) != 2 || !bytes.Equal(units[0], key) || !bytes.Equal(units[1], inter) || stamps[1] != int64(len(key))*1000 || skipped != 0 {
			t.Fatalf("one-byte reads: units=%x stamps=%v skipped=%d", units, stamps, skipped)
		}
	}
}

func TestH264AssemblerSkipsLeadingContinuation(t *testing.T) {
	key := annexB([]byte{0x65, 0xB8})
	units, stamps, fragments, skipped := assembleTestChunks(t, [][]byte{{0xAA, 0xBB, 0xCC, 0xDD}, key})
	if len(units) != 1 || !bytes.Equal(units[0], key) || stamps[0] != 1000 || fragments != 0 || skipped != 1 {
		t.Fatalf("units=%x stamps=%v fragments=%d skipped=%d", units, stamps, fragments, skipped)
	}
}

func TestH264AssemblerBoundsOrphanBuffer(t *testing.T) {
	skipped := 0
	a := h264Assembler{
		emit: func([]byte, int64) error { t.Fatal("orphan data emitted"); return nil },
		skip: func(cameraIndexLine) { skipped++ },
	}
	for i := 1; i <= 20; i++ {
		if err := a.add(cameraIndexLine{}, i, make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
		if len(a.data) > 3 || len(a.chunks) > 1 {
			t.Fatalf("orphan bytes accumulated: %d bytes, %d chunks", len(a.data), len(a.chunks))
		}
	}
	if err := a.flush(); err != nil {
		t.Fatal(err)
	}
	if skipped != 20 {
		t.Fatalf("skipped=%d, want 20", skipped)
	}
}

func TestFragmentedConvertSourceInPlace(t *testing.T) {
	key := annexB(mustHex(t, realSPS), mustHex(t, realPPS), []byte{0x65, 0xB8, 0xAA, 0xBB})
	inter := annexB([]byte{0x41, 0xC0, 0xCC})
	chunks := [][]byte{key[:len(key)-2], key[len(key)-2 : len(key)-1], append(bytes.Clone(key[len(key)-1:]), inter...), inter}
	dir, src := writeSingleSourceEpisode(t, []int64{0, 10_000_000, 20_000_000, 50_000_000}, chunks)
	before := snapshot(t, dir)
	r, err := ConvertSourceInPlace(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if r.IndexLines != 4 || r.Frames != 3 || r.Fragments != 2 || r.Skipped != 0 || r.UndecodedSliceHeaders != 0 {
		t.Fatalf("result = %+v", r)
	}
	assertMP4SampleBytes(t, r.Output, [][]byte{key, inter, inter})
	assertSampleDurations(t, r.Output, []time.Duration{20 * time.Millisecond, 30 * time.Millisecond})
	if err := os.Remove(r.Output); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatal("conversion modified the raw recording")
	}
}

func TestFragmentedConvertContinuesAfterBadIndexRange(t *testing.T) {
	for _, failure := range []string{"unreadable", "unparsed", "different segment"} {
		t.Run(failure, func(t *testing.T) {
			key := annexB(mustHex(t, realSPS), mustHex(t, realPPS), []byte{0x65, 0xB8, 0xAA})
			inter := annexB([]byte{0x41, 0xC0, 0xDD})
			dir, src := writeSingleSourceEpisode(t, []int64{0, 1, 2, 30_000_000}, [][]byte{key, {0xBB}, {0xCC}, inter})
			path := filepath.Join(src, "index.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			var rec cameraIndexLine
			if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "unreadable":
				rec.ByteSize = 100000
			case "different segment":
				rec.Segment = "cameras/cam/segment-000002.h264"
				rec.ByteOffset = 0
				if err := os.WriteFile(filepath.Join(src, "segment-000002.h264"), []byte{0xBB}, 0600); err != nil {
					t.Fatal(err)
				}
			}
			encoded, _ := json.Marshal(rec)
			lines[1] = string(encoded)
			if failure == "unparsed" {
				lines[1] = "{not valid json}"
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			r, err := ConvertSourceInPlace(dir, src)
			if err != nil {
				t.Fatal(err)
			}
			wantSkipped := 2
			if failure == "unparsed" {
				wantSkipped = 1
			}
			if r.Frames != 2 || r.Skipped != wantSkipped || r.Fragments != 0 || !strings.Contains(r.SkippedReason, "offset") {
				t.Fatalf("result = %+v", r)
			}
			assertMP4SampleBytes(t, r.Output, [][]byte{key, inter})
		})
	}
}

func assertMP4SampleBytes(t *testing.T, path string, units [][]byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stsz, co64 := bytes.Index(b, []byte("stsz")), bytes.Index(b, []byte("co64"))
	if stsz < 0 || co64 < 0 || int(binary.BigEndian.Uint32(b[stsz+12:])) != len(units) {
		t.Fatal("missing or incorrect MP4 sample table")
	}
	for i, unit := range units {
		var want []byte
		for _, nal := range splitAnnexB(unit) {
			switch nal[0] & 0x1f {
			case 7, 8, 9, 12:
				continue
			}
			want = append(want, u32(uint32(len(nal)))...)
			want = append(want, nal...)
		}
		offset := binary.BigEndian.Uint64(b[co64+12+i*8:])
		size := binary.BigEndian.Uint32(b[stsz+16+i*4:])
		if got := b[offset : offset+uint64(size)]; !bytes.Equal(got, want) {
			t.Fatalf("sample %d = %x; want %x", i, got, want)
		}
	}
}
