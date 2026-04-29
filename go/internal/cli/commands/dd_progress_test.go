//go:build darwin || linux

package commands

import (
	"strings"
	"testing"
)

func TestScanDDProgress_Linux(t *testing.T) {
	// GNU dd `status=progress` updates with '\r' and finishes with '\n':
	//   <bytes> bytes (<a> <unit>, <b> <unit>) copied, <s> s, <r>/s
	input := "" +
		"131072000 bytes (131 MB, 125 MiB) copied, 0.5 s, 262 MB/s\r" +
		"262144000 bytes (262 MB, 250 MiB) copied, 1.0 s, 262 MB/s\r" +
		"524288000 bytes (524 MB, 500 MiB) copied, 2.0 s, 262 MB/s\n" +
		"100+0 records in\n" +
		"100+0 records out\n"

	var got []int64
	scanDDProgress(strings.NewReader(input), func(written int64) {
		got = append(got, written)
	})

	want := []int64{131072000, 262144000, 524288000}
	if len(got) != len(want) {
		t.Fatalf("got %d updates (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("update %d: got %d, want %d", i, got[i], w)
		}
	}
}

func TestScanDDProgress_macOS(t *testing.T) {
	// BSD dd `status=progress` (Monterey+) updates with '\r' and ends with
	// a newline-terminated three-line summary on completion. The mid-stream
	// format omits "in":
	//   <bytes> bytes (<a> <unit>, <b> <unit>) transferred <s>s, <r>/s
	// The final summary line uses "transferred in" instead.
	input := "" +
		"73519857664 bytes (74 GB, 68 GiB) transferred 1.004s, 73 GB/s\r" +
		"146314100736 bytes (146 GB, 136 GiB) transferred 1.998s, 73 GB/s\r" +
		"100+0 records in\n" +
		"100+0 records out\n" +
		"209715200 bytes transferred in 2.500 secs (83886080 bytes/sec)\n"

	var got []int64
	scanDDProgress(strings.NewReader(input), func(written int64) {
		got = append(got, written)
	})

	want := []int64{73519857664, 146314100736, 209715200}
	if len(got) != len(want) {
		t.Fatalf("got %d updates (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("update %d: got %d, want %d", i, got[i], w)
		}
	}
}

func TestScanDDProgress_IgnoresNonNumericFirstToken(t *testing.T) {
	// "records in" / "records out" lines have a "+0" suffix on the first
	// token so ParseInt rejects them and we skip silently.
	input := "starting up...\n100+0 records in\n100+0 records out\n"

	var got []int64
	scanDDProgress(strings.NewReader(input), func(written int64) {
		got = append(got, written)
	})

	if len(got) != 0 {
		t.Errorf("expected no progress updates, got %v", got)
	}
}

func TestScanDDProgress_NilCallback(t *testing.T) {
	// Should drain the reader without panicking when progressFn is nil.
	scanDDProgress(strings.NewReader("anything"), nil)
}
