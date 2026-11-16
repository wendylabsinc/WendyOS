package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Synthetic NAL units, without start codes. The parameter sets are well formed
// as far as anything here reads them (each PPS carries a real
// pic_parameter_set_id); the slices are a header byte and filler.
var (
	h264TestSPS     = []byte{0x67, 0x42, 0xc0, 0x1e, 0xd9, 0x00, 0xa0, 0x47, 0xfe, 0x88}
	h264TestSPSNew  = []byte{0x67, 0x64, 0x00, 0x28, 0xac, 0xd9, 0x40, 0x78, 0x02, 0x27}
	h264TestPPS0    = []byte{0x68, 0xce, 0x3c, 0x80} // pic_parameter_set_id 0
	h264TestPPS0New = []byte{0x68, 0xce, 0x38, 0x80} // id 0 again, different content
	h264TestPPS1    = []byte{0x68, 0x5e, 0x3c, 0x80} // pic_parameter_set_id 1
	h264TestIDR     = []byte{0x65, 0x88, 0x84, 0x21, 0xa0}
	h264TestInter   = []byte{0x41, 0x9a, 0x24, 0x6c}
	h264TestAUD     = []byte{0x09, 0x10}
	h264TestSEI     = []byte{0x06, 0x05, 0x04, 0xde, 0xad, 0xbe, 0xef, 0x80}
)

// h264Stream joins NAL units into an Annex-B byte stream, each behind a start
// code of codeLen bytes (3 or 4).
func h264Stream(codeLen int, nals ...[]byte) []byte {
	var out []byte
	for _, nal := range nals {
		out = append(out, []byte{0, 0, 0, 1}[4-codeLen:]...)
		out = append(out, nal...)
	}
	return out
}

func h264Joined(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// requireSameBuffer fails unless got is the very slice that went in (same
// backing array, length and capacity) and its bytes are what they were.
func requireSameBuffer(t *testing.T, got, in, before []byte) {
	t.Helper()
	if len(got) != len(in) || cap(got) != cap(in) || (len(in) > 0 && &got[0] != &in[0]) {
		t.Fatalf("buffer was reallocated or resliced: got len %d cap %d, want the %d-byte input itself", len(got), cap(got), len(in))
	}
	if !bytes.Equal(in, before) {
		t.Fatalf("buffer was modified in place:\n got %x\nwant %x", in, before)
	}
}

// requireRandomAccess fails unless the consumers' own test (the one
// ensureSegment, the pre-roll ring and the reattach gates apply) accepts au as
// a unit decoding can start on, beginning at offset.
func requireRandomAccess(t *testing.T, au []byte, offset int) {
	t.Helper()
	got, found := h264RandomAccessOffset(au)
	if !found || got != offset {
		t.Fatalf("h264RandomAccessOffset = %d, %v; want %d, true for %x", got, found, offset, au)
	}
}

// rememberedSets returns a cache that has seen one SPS and PPS 0, the way a
// camera sends them once when the stream starts.
func rememberedSets() *h264ParameterSets {
	sets := &h264ParameterSets{}
	sets.inline(h264Stream(4, h264TestSPS, h264TestPPS0), true)
	return sets
}

// A camera that already puts SPS and PPS ahead of every IDR must see no
// difference at all: the buffer the capture loop allocated goes on to the hub
// as it is, whatever the cache holds.
func TestH264InlineLeavesSelfContainedIDRUntouched(t *testing.T) {
	cases := map[string][]byte{
		"four-byte start codes":  h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR),
		"three-byte start codes": h264Stream(3, h264TestSPS, h264TestPPS0, h264TestIDR),
		"delimiter and SEI":      h264Stream(4, h264TestAUD, h264TestSPS, h264TestPPS0, h264TestSEI, h264TestIDR),
		"two PPS":                h264Stream(4, h264TestSPS, h264TestPPS0, h264TestPPS1, h264TestIDR),
		"several IDR slices":     h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR, h264TestIDR),
	}
	for name, au := range cases {
		t.Run(name, func(t *testing.T) {
			// Primed with other parameter sets, as after a mid-stream change: what
			// is remembered must never displace what the unit itself carries.
			sets := &h264ParameterSets{}
			sets.inline(h264Stream(4, h264TestSPSNew, h264TestPPS0New), true)
			before := bytes.Clone(au)
			for pass := 1; pass <= 2; pass++ {
				got, result := sets.inline(au, true)
				if result != h264Unchanged {
					t.Fatalf("pass %d: result = %v, want h264Unchanged", pass, result)
				}
				requireSameBuffer(t, got, au, before)
			}
		})
	}
}

// The defect: parameter sets sent once, in their own buffer, before the first
// IDR. Every later IDR must come out as SPS, PPS, IDR.
func TestH264InlineRepeatsParameterSetsSentOnceAtStreamStart(t *testing.T) {
	sets := &h264ParameterSets{}
	start := h264Stream(4, h264TestSPS, h264TestPPS0)
	before := bytes.Clone(start)
	got, result := sets.inline(start, true)
	if result != h264Unchanged {
		t.Fatalf("parameter-set buffer: result = %v, want h264Unchanged", result)
	}
	requireSameBuffer(t, got, start, before)

	want := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)
	for gop := 1; gop <= 3; gop++ {
		idr := h264Stream(4, h264TestIDR)
		got, result := sets.inline(idr, true)
		if result != h264Inlined || !bytes.Equal(got, want) {
			t.Fatalf("IDR %d: result = %v\n got %x\nwant %x", gop, result, got, want)
		}
		requireRandomAccess(t, got, 0)
		if !bytes.Equal(idr, h264Stream(4, h264TestIDR)) {
			t.Fatalf("IDR %d: the camera's own buffer was modified: %x", gop, idr)
		}

		inter := h264Stream(4, h264TestInter)
		interBefore := bytes.Clone(inter)
		got, result = sets.inline(inter, true)
		if result != h264Unchanged {
			t.Fatalf("inter frame %d: result = %v, want h264Unchanged", gop, result)
		}
		requireSameBuffer(t, got, inter, interBefore)
	}
}

func TestH264InlineKeepsEveryPPSID(t *testing.T) {
	sets := &h264ParameterSets{}
	sets.inline(h264Stream(4, h264TestSPS, h264TestPPS1), true)
	// A second PPS arrives later, by itself.
	sets.inline(h264Stream(4, h264TestPPS0), true)

	got, result := sets.inline(h264Stream(4, h264TestIDR), true)
	want := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestPPS1, h264TestIDR)
	if result != h264Inlined || !bytes.Equal(got, want) {
		t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
	}
	requireRandomAccess(t, got, 0)
}

func TestH264InlineUsesTheNewestParameterSets(t *testing.T) {
	t.Run("sent in their own buffer", func(t *testing.T) {
		sets := &h264ParameterSets{}
		sets.inline(h264Stream(4, h264TestSPS, h264TestPPS0, h264TestPPS1), true)
		// The camera reconfigures mid-stream: a new SPS and a new PPS 0. PPS 1
		// is not sent again, so it stays as it was.
		sets.inline(h264Stream(4, h264TestSPSNew, h264TestPPS0New), true)

		got, result := sets.inline(h264Stream(4, h264TestIDR), true)
		want := h264Stream(4, h264TestSPSNew, h264TestPPS0New, h264TestPPS1, h264TestIDR)
		if result != h264Inlined || !bytes.Equal(got, want) {
			t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
		}
	})
	t.Run("sent inside an IDR", func(t *testing.T) {
		sets := rememberedSets()
		sets.inline(h264Stream(4, h264TestSPSNew, h264TestPPS0New, h264TestIDR), true)

		got, result := sets.inline(h264Stream(4, h264TestIDR), true)
		want := h264Stream(4, h264TestSPSNew, h264TestPPS0New, h264TestIDR)
		if result != h264Inlined || !bytes.Equal(got, want) {
			t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
		}
	})
}

// An IDR that carries an SPS or a PPS but not both, or the two in an order
// decoding cannot start from, gets the remembered sets ahead of the IDR so an
// SPS is followed by a PPS and then the picture.
func TestH264InlineCompletesAnIDRThatCarriesOnlyOneKind(t *testing.T) {
	remembered := h264Stream(4, h264TestSPS, h264TestPPS0)
	cases := []struct {
		name         string
		in, want     []byte
		randomAccess int
	}{
		{
			name:         "SPS without PPS",
			in:           h264Stream(4, h264TestSPS, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestSPS), remembered, h264Stream(4, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestSPS)),
		},
		{
			name:         "PPS without SPS",
			in:           h264Stream(4, h264TestPPS0, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestPPS0), remembered, h264Stream(4, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestPPS0)),
		},
		{
			name:         "PPS ahead of SPS",
			in:           h264Stream(4, h264TestPPS0, h264TestSPS, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestPPS0, h264TestSPS), remembered, h264Stream(4, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestPPS0, h264TestSPS)),
		},
		{
			name:         "a new SPS without PPS",
			in:           h264Stream(4, h264TestSPSNew, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestSPSNew), h264Stream(4, h264TestSPSNew, h264TestPPS0), h264Stream(4, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestSPSNew)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, result := rememberedSets().inline(tc.in, true)
			if result != h264Inlined || !bytes.Equal(got, tc.want) {
				t.Fatalf("result = %v\n got %x\nwant %x", result, got, tc.want)
			}
			requireRandomAccess(t, got, tc.randomAccess)
		})
	}
}

func TestH264InlineReadsThreeAndFourByteStartCodes(t *testing.T) {
	// What is inserted always has four-byte start codes, which a parameter set
	// and the first NAL unit of an access unit require. The camera's own bytes
	// follow as they were.
	inserted := h264Stream(4, h264TestSPS, h264TestPPS0)
	for _, codeLen := range []int{3, 4} {
		sets := &h264ParameterSets{}
		sets.inline(h264Stream(codeLen, h264TestSPS, h264TestPPS0), true)
		got, result := sets.inline(h264Stream(codeLen, h264TestIDR), true)
		want := h264Joined(inserted, h264Stream(codeLen, h264TestIDR))
		if result != h264Inlined || !bytes.Equal(got, want) {
			t.Fatalf("%d-byte start codes: result = %v\n got %x\nwant %x", codeLen, result, got, want)
		}
		requireRandomAccess(t, got, 0)
	}

	sets := &h264ParameterSets{}
	sets.inline(h264Joined(h264Stream(3, h264TestSPS), h264Stream(4, h264TestPPS0)), true)
	got, result := sets.inline(h264Stream(3, h264TestIDR), true)
	want := h264Joined(inserted, h264Stream(3, h264TestIDR))
	if result != h264Inlined || !bytes.Equal(got, want) {
		t.Fatalf("mixed start codes: result = %v\n got %x\nwant %x", result, got, want)
	}
}

// Parameter sets go after an access unit delimiter, which has to stay first,
// and ahead of SEI, which can depend on the SPS.
func TestH264InlineInsertsAfterTheDelimiterAndAheadOfSEI(t *testing.T) {
	inserted := h264Stream(4, h264TestSPS, h264TestPPS0)
	cases := []struct {
		name         string
		in, want     []byte
		randomAccess int
	}{
		{
			name:         "delimiter",
			in:           h264Stream(4, h264TestAUD, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestAUD), inserted, h264Stream(4, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestAUD)),
		},
		{
			name:         "delimiter and SEI",
			in:           h264Stream(4, h264TestAUD, h264TestSEI, h264TestIDR),
			want:         h264Joined(h264Stream(4, h264TestAUD), inserted, h264Stream(4, h264TestSEI, h264TestIDR)),
			randomAccess: len(h264Stream(4, h264TestAUD)),
		},
		{
			name:         "delimiter and SEI behind three-byte start codes",
			in:           h264Stream(3, h264TestAUD, h264TestSEI, h264TestIDR),
			want:         h264Joined(h264Stream(3, h264TestAUD), inserted, h264Stream(3, h264TestSEI, h264TestIDR)),
			randomAccess: len(h264Stream(3, h264TestAUD)),
		},
		{
			name: "SEI",
			in:   h264Stream(4, h264TestSEI, h264TestIDR),
			want: h264Joined(inserted, h264Stream(4, h264TestSEI, h264TestIDR)),
		},
		{
			name:         "bytes ahead of the first start code",
			in:           h264Joined([]byte{9, 9, 9}, h264Stream(3, h264TestIDR)),
			want:         h264Joined([]byte{9, 9, 9}, inserted, h264Stream(3, h264TestIDR)),
			randomAccess: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, result := rememberedSets().inline(tc.in, true)
			if result != h264Inlined || !bytes.Equal(got, tc.want) {
				t.Fatalf("result = %v\n got %x\nwant %x", result, got, tc.want)
			}
			requireRandomAccess(t, got, tc.randomAccess)
		})
	}
}

// A buffer the maxFrameBytes cap cut short is not a whole access unit, so
// nothing is added to it.
func TestH264InlineLeavesATruncatedBufferAlone(t *testing.T) {
	idr := h264Stream(4, h264TestIDR)
	before := bytes.Clone(idr)
	got, result := rememberedSets().inline(idr, false)
	if result != h264Unchanged {
		t.Fatalf("result = %v, want h264Unchanged", result)
	}
	requireSameBuffer(t, got, idr, before)
}

// A truncated buffer's last NAL unit may itself be cut short, so it is never
// remembered; the units ahead of it are whole and are.
func TestH264InlineDoesNotRememberAParameterSetCutShort(t *testing.T) {
	sets := &h264ParameterSets{}
	sets.inline(h264Stream(4, h264TestSPS, h264TestPPS0[:2]), false)
	if got, result := sets.inline(h264Stream(4, h264TestIDR), true); result != h264MissingParameterSets {
		t.Fatalf("result = %v, want h264MissingParameterSets; the cut-short PPS was used: %x", result, got)
	}

	sets.inline(h264Stream(4, h264TestPPS0), true)
	got, result := sets.inline(h264Stream(4, h264TestIDR), true)
	want := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)
	if result != h264Inlined || !bytes.Equal(got, want) {
		t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
	}
}

func TestH264InlineReportsAnIDRItCannotComplete(t *testing.T) {
	cases := []struct {
		name          string
		seen, arrives []byte
	}{
		{name: "nothing seen", arrives: h264Stream(4, h264TestSPS, h264TestPPS0)},
		{name: "SPS only", seen: h264Stream(4, h264TestSPS), arrives: h264Stream(4, h264TestPPS0)},
		{name: "PPS only", seen: h264Stream(4, h264TestPPS0), arrives: h264Stream(4, h264TestSPS)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sets := &h264ParameterSets{}
			sets.inline(tc.seen, true)

			idr := h264Stream(4, h264TestIDR)
			before := bytes.Clone(idr)
			got, result := sets.inline(idr, true)
			if result != h264MissingParameterSets {
				t.Fatalf("result = %v, want h264MissingParameterSets", result)
			}
			requireSameBuffer(t, got, idr, before)

			sets.inline(tc.arrives, true)
			got, result = sets.inline(h264Stream(4, h264TestIDR), true)
			want := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)
			if result != h264Inlined || !bytes.Equal(got, want) {
				t.Fatalf("after the missing set arrived: result = %v\n got %x\nwant %x", result, got, want)
			}
		})
	}
}

// The hub drops a frame larger than maxFrameBytes. An IDR that fits today must
// not be pushed over that limit, or every viewer would lose a keyframe they
// used to get.
func TestH264InlineNeverGrowsAFramePastTheHubLimit(t *testing.T) {
	sets := rememberedSets()
	added := len(h264Stream(4, h264TestSPS, h264TestPPS0))
	idrOfSize := func(size int) []byte {
		au := h264Stream(4, h264TestIDR)
		return append(au, bytes.Repeat([]byte{0xff}, size-len(au))...)
	}

	fits := idrOfSize(maxFrameBytes - added)
	got, result := sets.inline(fits, true)
	if result != h264Inlined || len(got) != maxFrameBytes {
		t.Fatalf("result = %v, len %d; want h264Inlined at exactly %d bytes", result, len(got), maxFrameBytes)
	}
	if frameTooLarge(&videoFrame{data: got, codec: agentpb.VideoCodec_VIDEO_CODEC_H264}) {
		t.Fatal("the hub would drop the completed frame")
	}

	tooBig := idrOfSize(maxFrameBytes - added + 1)
	before := bytes.Clone(tooBig)
	got, result = sets.inline(tooBig, true)
	if result != h264MissingParameterSets {
		t.Fatalf("result = %v, want h264MissingParameterSets", result)
	}
	requireSameBuffer(t, got, tooBig, before)
}

// A misbehaving camera must not be able to make the agent hold, or add to every
// keyframe, an unbounded amount of "parameter set".
func TestH264InlineBoundsWhatItRemembers(t *testing.T) {
	sets := rememberedSets()
	huge := append(bytes.Clone(h264TestPPS0), bytes.Repeat([]byte{0xff}, h264ParameterSetCacheBytes)...)
	sets.inline(h264Stream(4, huge), true)
	if sets.size > h264ParameterSetCacheBytes {
		t.Fatalf("remembered %d bytes, over the %d-byte bound", sets.size, h264ParameterSetCacheBytes)
	}
	// The PPS 0 it would have replaced is gone as well: the camera has moved on
	// from it, so it is no longer the answer.
	if got, result := sets.inline(h264Stream(4, h264TestIDR), true); result != h264MissingParameterSets {
		t.Fatalf("result = %v, want h264MissingParameterSets; got %d bytes", result, len(got))
	}

	sets.inline(h264Stream(4, h264TestPPS0), true)
	got, result := sets.inline(h264Stream(4, h264TestIDR), true)
	want := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)
	if result != h264Inlined || !bytes.Equal(got, want) {
		t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
	}
}

func TestH264InlineLeavesOtherBuffersUntouched(t *testing.T) {
	cases := map[string][]byte{
		"inter frame":               h264Stream(4, h264TestInter),
		"delimiter and inter frame": h264Stream(3, h264TestAUD, h264TestInter),
		"SEI by itself":             h264Stream(4, h264TestSEI),
		// Two pictures in one buffer is not what V4L2 promises; it is not an
		// access unit this code knows how to complete.
		"inter frame ahead of an IDR": h264Stream(4, h264TestInter, h264TestIDR),
		"no start code":               {1, 2, 3, 4, 5},
		"start code and nothing else": {0, 0, 0, 1},
		"empty":                       {},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			before := bytes.Clone(in)
			got, result := rememberedSets().inline(in, true)
			if result != h264Unchanged {
				t.Fatalf("result = %v, want h264Unchanged", result)
			}
			requireSameBuffer(t, got, in, before)
		})
	}
}

// Some encoders pad a NAL unit with zero bytes ahead of the next start code. A
// set is remembered as the camera delimited it, padding included, so the copy
// inserted later is the same unit a reader split out of the camera's own (the
// episode export compares them byte for byte). Only the buffer's last unit has
// nothing to delimit it, and there the zeros are dropped.
func TestH264InlineKeepsAParameterSetAsTheCameraDelimitedIt(t *testing.T) {
	paddedSPS := h264Joined(h264TestSPS, []byte{0, 0})
	sets := &h264ParameterSets{}
	sets.inline(h264Joined(h264Stream(4, paddedSPS, h264TestPPS0), []byte{0, 0, 0}), true)
	got, result := sets.inline(h264Stream(4, h264TestIDR), true)
	want := h264Stream(4, paddedSPS, h264TestPPS0, h264TestIDR)
	if result != h264Inlined || !bytes.Equal(got, want) {
		t.Fatalf("result = %v\n got %x\nwant %x", result, got, want)
	}
	requireRandomAccess(t, got, 0)
}

func TestH264PPSID(t *testing.T) {
	cases := []struct {
		name string
		nal  []byte
		id   int
		ok   bool
	}{
		{"id 0", h264TestPPS0, 0, true},
		{"id 1", h264TestPPS1, 1, true},
		{"id 2", []byte{0x68, 0x7e}, 2, true},
		{"id 3", []byte{0x68, 0x27}, 3, true},
		{"id 255, the largest", []byte{0x68, 0x00, 0x80, 0x40}, 255, true},
		{"id 256", []byte{0x68, 0x00, 0x80, 0x80}, 0, false},
		{"more leading zeros than any id has", []byte{0x68, 0x00, 0x40, 0x00, 0x00}, 0, false},
		{"header only", []byte{0x68}, 0, false},
		{"ends inside the leading zeros", []byte{0x68, 0x00}, 0, false},
		{"ends inside the id", []byte{0x68, 0x00, 0x80}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range cases {
		id, ok := h264PPSID(tc.nal)
		if id != tc.id || ok != tc.ok {
			t.Errorf("%s: h264PPSID(%x) = %d, %v; want %d, %v", tc.name, tc.nal, id, ok, tc.id, tc.ok)
		}
	}
}

func TestH264NextNAL(t *testing.T) {
	type unit struct{ start, header int }
	cases := []struct {
		name    string
		payload []byte
		want    []unit
	}{
		{"three-byte start code", []byte{0, 0, 1, 0x65}, []unit{{0, 3}}},
		{"four-byte start code", []byte{0, 0, 0, 1, 0x65}, []unit{{0, 4}}},
		{"an extra leading zero", []byte{0, 0, 0, 0, 1, 0x65}, []unit{{1, 5}}},
		{"two units after stray bytes", []byte{9, 9, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68}, []unit{{2, 5}, {7, 11}}},
		{"start code with no header byte", []byte{0x65, 0, 0, 1}, nil},
		{"no start code", []byte{0x65, 1, 2, 3}, nil},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		var got []unit
		for start, header, ok := h264NextNAL(tc.payload, 0); ok; start, header, ok = h264NextNAL(tc.payload, header+1) {
			got = append(got, unit{start, header})
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: units = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: units = %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// h264RandomAccessOffset is the test every consumer applies. Its scan moved
// into h264NextNAL; these are the answers it gave before and must still give.
func TestH264RandomAccessOffsetCases(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		offset  int
		found   bool
	}{
		{"SPS, PPS, IDR", h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR), 0, true},
		{"three-byte start codes", h264Stream(3, h264TestSPS, h264TestPPS0, h264TestIDR), 0, true},
		{"after a delimiter", h264Stream(4, h264TestAUD, h264TestSPS, h264TestPPS0, h264TestIDR), 6, true},
		{"SEI between PPS and IDR", h264Stream(4, h264TestSPS, h264TestPPS0, h264TestSEI, h264TestIDR), 0, true},
		{"the later of two SPS", h264Stream(4, h264TestSPS, h264TestSPSNew, h264TestPPS0, h264TestIDR), 14, true},
		{"IDR alone", h264Stream(4, h264TestIDR), 0, false},
		{"SPS without PPS", h264Stream(4, h264TestSPS, h264TestIDR), 0, false},
		{"PPS without SPS", h264Stream(4, h264TestPPS0, h264TestIDR), 0, false},
		{"PPS ahead of SPS", h264Stream(4, h264TestPPS0, h264TestSPS, h264TestIDR), 0, false},
		{"parameter sets without a picture", h264Stream(4, h264TestSPS, h264TestPPS0), 0, false},
		{"inter frame", h264Stream(4, h264TestSPS, h264TestPPS0, h264TestInter), 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range cases {
		offset, found := h264RandomAccessOffset(tc.payload)
		if offset != tc.offset || found != tc.found {
			t.Errorf("%s: h264RandomAccessOffset = %d, %v; want %d, %v", tc.name, offset, found, tc.offset, tc.found)
		}
	}
}

// nativeTestCapture stands in for one native V4L2 H.264 capture feeding hub:
// the emit callback streamV4L2Native gives the capture loop, delivering through
// produce the way runProducer wires it. Each call is one dequeued buffer; whole
// is false for one the maxFrameBytes cap cut short.
func nativeTestCapture(svc *VideoService, hub *deviceHub) func(buffer []byte, whole bool) bool {
	emit := svc.nativeH264Emit("/dev/video0", func(payload []byte, stamp frameTimestamp, codec agentpb.VideoCodec) bool {
		return hub.produce(&videoFrame{data: payload, tsNs: stamp.wallNs, codec: codec, nativeNs: stamp.nativeNs, nativeClock: stamp.nativeClock, nativeFlags: stamp.nativeFlags, sequence: stamp.sequence, sequenceValid: stamp.sequenceValid, auAligned: stamp.auAligned})
	})
	var sequence uint32
	return func(buffer []byte, whole bool) bool {
		stamp := realtimeFrameTimestamp(time.Now())
		stamp.sequence, stamp.sequenceValid, stamp.auAligned = sequence, true, whole
		sequence++
		return emit(buffer, stamp)
	}
}

// The consumers' side of the defect. A camera sends SPS and PPS once, then bare
// IDRs. Everyone who joins the running hub afterwards (a viewer, an episode
// recording, a sensor subscription behind its random-access gate, a pre-roll
// ring, the loopback pump after a rejoin) must be able to start on the next
// IDR, and all of them must still name that frame identically.
func TestLateJoinersStartOnAnIDRTheCameraSentWithoutParameterSets(t *testing.T) {
	svc := &VideoService{logger: zap.NewNop()}
	seq := new(atomic.Uint64)
	hub, cancel := newSampleHub(seq)
	defer cancel()
	camera := nativeTestCapture(svc, hub)

	// Someone is there from the start and keeps the producer running, the way an
	// armed campaign does.
	firstID, first := subscribeDeep(t, hub)
	camera(h264Stream(4, h264TestSPS, h264TestPPS0), true) // sample 1
	camera(h264Stream(4, h264TestIDR), true)               // sample 2
	camera(h264Stream(4, h264TestInter), true)             // sample 3

	// Everyone below joins after the parameter sets have gone by.
	viewerID, viewer := subscribeDeep(t, hub)
	_, recorderFrames := subscribeDeep(t, hub)
	sensorID, sensorFrames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	_, ringFrames := subscribeDeep(t, hub)
	pumpID, pumpFrames, err := hub.subscribe()
	if err != nil {
		t.Fatal(err)
	}

	camera(h264Stream(4, h264TestInter), true) // sample 4
	camera(h264Stream(4, h264TestIDR), true)   // sample 5: the IDR they can start on
	camera(h264Stream(4, h264TestInter), true) // sample 6

	const keyframeSample = 5
	keyframe := h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)
	inter := h264Stream(4, h264TestInter)

	// One buffer in, one frame out: completing an IDR must not add or remove a
	// frame, or every sample_id after it would name a different picture.
	if got := seq.Load(); got != 6 {
		t.Fatalf("six buffers consumed %d sample identities, want 6", got)
	}

	t.Run("viewer", func(t *testing.T) {
		sent := pumpClosed(hub, viewerID, viewer)
		want := [][]byte{inter, keyframe, inter}
		if len(sent) != len(want) {
			t.Fatalf("viewer was sent %d messages, want %d", len(sent), len(want))
		}
		for i := range want {
			if !bytes.Equal(sent[i], want[i]) {
				t.Fatalf("message %d:\n got %x\nwant %x", i, sent[i], want[i])
			}
		}
		requireRandomAccess(t, sent[1], 0)
	})

	t.Run("first viewer", func(t *testing.T) {
		// Whoever was there from the start is sent the same frames; the repeated
		// parameter sets are ones it already has.
		sent := pumpClosed(hub, firstID, first)
		want := [][]byte{h264Stream(4, h264TestSPS, h264TestPPS0), keyframe, inter, inter, keyframe, inter}
		if len(sent) != len(want) {
			t.Fatalf("first viewer was sent %d messages, want %d", len(sent), len(want))
		}
		for i := range want {
			if !bytes.Equal(sent[i], want[i]) {
				t.Fatalf("message %d:\n got %x\nwant %x", i, sent[i], want[i])
			}
		}
	})

	t.Run("episode recording", func(t *testing.T) {
		c := newTestCameraCapture(t, &fakeReceipt{now: int64(time.Second)})
		var outcomes []error
		for len(recorderFrames) > 0 {
			outcomes = append(outcomes, c.handleFrame(<-recorderFrames))
		}
		if len(outcomes) != 3 || !errors.Is(outcomes[0], errAwaitCameraRandomAccess) || outcomes[1] != nil || outcomes[2] != nil {
			t.Fatalf("handleFrame outcomes = %v; want the inter frame refused, then the IDR and what follows written", outcomes)
		}
		if err := c.segment.Sync(); err != nil {
			t.Fatal(err)
		}
		defer c.segment.Close()
		media, err := os.ReadFile(filepath.Join(c.dir, "segment-000001.h264"))
		if err != nil {
			t.Fatal(err)
		}
		if want := h264Joined(keyframe, inter); !bytes.Equal(media, want) {
			t.Fatalf("segment:\n got %x\nwant %x", media, want)
		}
		contents, err := os.ReadFile(filepath.Join(c.dir, "index.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var opening cameraIndexRecord
		if err := json.Unmarshal(bytes.SplitN(contents, []byte("\n"), 2)[0], &opening); err != nil {
			t.Fatal(err)
		}
		if opening.SampleID != keyframeSample || opening.ByteOffset != 0 || opening.ByteSize != len(keyframe) {
			t.Fatalf("opening index entry = sample %d offset %d size %d; want sample %d offset 0 size %d",
				opening.SampleID, opening.ByteOffset, opening.ByteSize, keyframeSample, len(keyframe))
		}
	})

	t.Run("sensor subscription", func(t *testing.T) {
		subscription := &cameraSensorSubscription{hub: hub, subID: sensorID, frames: sensorFrames, awaitRandomAccess: true}
		// Bounded: a gate that never opens is the defect, and must fail the test
		// rather than hang it.
		ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		sample, err := subscription.Next(ctx)
		if err != nil {
			t.Fatalf("the random-access gate never opened: %v", err)
		}
		if sample.SampleID != keyframeSample || !sample.SelfContained || sample.DroppedBefore != 1 {
			t.Fatalf("sample = id %d selfContained %v droppedBefore %d; want id %d, self-contained, 1 gated frame before it",
				sample.SampleID, sample.SelfContained, sample.DroppedBefore, keyframeSample)
		}
		// The model-input ledger records this length as payload_bytes beside the
		// capture index's byte_size for the same sample.
		if !bytes.Equal(sample.Payload, keyframe) {
			t.Fatalf("payload:\n got %x\nwant %x", sample.Payload, keyframe)
		}
	})

	t.Run("pre-roll ring", func(t *testing.T) {
		ring := &cameraPreRollRing{buffer: time.Minute, limitBytes: preRollCameraLimitBytes}
		for len(ringFrames) > 0 {
			ring.add(<-ringFrames)
		}
		if len(ring.frames) != 2 || ring.frames[0].frame.sampleID != keyframeSample || !ring.frames[0].randomAccess {
			t.Fatalf("ring holds %d frames; want it to open on sample %d and keep what follows", len(ring.frames), keyframeSample)
		}
		if want := len(keyframe) + len(inter); ring.bytes != want {
			t.Fatalf("ring accounts %d bytes, want %d", ring.bytes, want)
		}
	})

	t.Run("loopback pump", func(t *testing.T) {
		writer := newFakeLoopbackWriter(100)
		pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")
		close(pumpFrames)
		if _, err := pump.pumpFrom(context.Background(), hub, pumpID, pumpFrames, writer, true, 0); err != nil {
			t.Fatalf("pumpFrom: %v", err)
		}
		if len(writer.writes) != 2 || !bytes.Equal(writer.writes[0], keyframe) || writer.stamps[0] != keyframeSample {
			t.Fatalf("node was written %d frames, first %x stamped %v; want the completed IDR stamped %d, then the inter frame",
				len(writer.writes), writer.writes, writer.stamps, keyframeSample)
		}
		binding, ok := pump.Bindings().Lookup(100)
		if !ok || binding.SampleID != keyframeSample || binding.HubDropsBefore != 1 {
			t.Fatalf("binding = %+v, found %v; want sample %d with the one gated frame reported", binding, ok, keyframeSample)
		}
	})
}

// The remembered sets belong to one capture. A producer restart, or another
// camera, starts with none: it must not be handed a previous stream's SPS.
func TestNativeH264ParameterSetsBelongToOneCapture(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	svc := &VideoService{logger: zap.New(core)}
	newCamera := func() (func([]byte, bool) bool, chan *videoFrame) {
		hub, cancel := newSampleHub(new(atomic.Uint64))
		t.Cleanup(cancel)
		_, frames := subscribeDeep(t, hub)
		return nativeTestCapture(svc, hub), frames
	}
	delivered := func(frames chan *videoFrame) *videoFrame {
		t.Helper()
		select {
		case frame := <-frames:
			return frame
		default:
			t.Fatal("no frame was delivered")
			return nil
		}
	}
	bare := h264Stream(4, h264TestIDR)

	first, firstFrames := newCamera()
	first(h264Stream(4, h264TestSPS, h264TestPPS0), true)
	delivered(firstFrames)
	for i := 0; i < 2; i++ {
		first(bare, true)
		if got, want := delivered(firstFrames).data, h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR); !bytes.Equal(got, want) {
			t.Fatalf("first capture, IDR %d:\n got %x\nwant %x", i, got, want)
		}
	}
	if got := logs.FilterLevelExact(zapcore.InfoLevel).Len(); got != 1 {
		t.Fatalf("%d info lines for two completed IDRs, want one per capture", got)
	}
	if got := logs.FilterLevelExact(zapcore.WarnLevel).Len(); got != 0 {
		t.Fatalf("%d warnings for a capture that could complete every IDR", got)
	}

	// A new capture: the producer restarted, or this is another camera.
	second, secondFrames := newCamera()
	for i := 0; i < 3; i++ {
		second(bare, true)
		if got := delivered(secondFrames).data; !bytes.Equal(got, bare) {
			t.Fatalf("second capture, IDR %d, was given another capture's parameter sets: %x", i, got)
		}
	}
	warnings := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warnings) != 1 {
		t.Fatalf("%d warnings for three IDRs that could not be completed, want one per capture", len(warnings))
	}
	if device, _ := warnings[0].ContextMap()["device"].(string); device != "/dev/video0" {
		t.Fatalf("warning does not name the camera: %v", warnings[0].ContextMap())
	}

	// A truncated buffer goes through as it is, still marked as not whole.
	second(h264Stream(4, h264TestSPSNew, h264TestPPS0New), true)
	delivered(secondFrames)
	second(bare, false)
	if frame := delivered(secondFrames); !bytes.Equal(frame.data, bare) || frame.auAligned {
		t.Fatalf("truncated buffer: data %x aligned %v; want it forwarded as it was", frame.data, frame.auAligned)
	}

	// Once its own parameter sets have arrived, each capture uses its own.
	second(bare, true)
	if got, want := delivered(secondFrames).data, h264Stream(4, h264TestSPSNew, h264TestPPS0New, h264TestIDR); !bytes.Equal(got, want) {
		t.Fatalf("second capture:\n got %x\nwant %x", got, want)
	}
	first(bare, true)
	if frame := delivered(firstFrames); !bytes.Equal(frame.data, h264Stream(4, h264TestSPS, h264TestPPS0, h264TestIDR)) || !frame.auAligned {
		t.Fatalf("first capture: data %x aligned %v", frame.data, frame.auAligned)
	}
}
