package go2video

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"
)

func TestRecordedFrameMatchingRequiresFreshOrderedSourceContent(t *testing.T) {
	reference := make([][32]byte, 180)
	for i := range reference {
		reference[i] = sha256.Sum256([]byte{byte(i)})
	}
	for name, capture := range map[string][][32]byte{
		"ordered":       {reference[10], reference[11], reference[11], reference[13], reference[16], reference[18]},
		"loop boundary": {reference[177], reference[178], reference[179], reference[0], reference[1], reference[2]},
	} {
		t.Run(name, func(t *testing.T) {
			if err := MatchRecording(reference, capture); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, capture := range map[string][][32]byte{
		"stale":     {reference[5], reference[5], reference[5], reference[5], reference[5], reference[5]},
		"unrelated": {reference[1], reference[2], reference[3], sha256.Sum256([]byte("not in recording")), reference[5], reference[6]},
		"backwards": {reference[10], reference[11], reference[12], reference[9], reference[14], reference[15]},
		"too few":   {reference[1], reference[2], reference[3]},
	} {
		t.Run(name, func(t *testing.T) {
			if err := MatchRecording(reference, capture); err == nil {
				t.Fatal("invalid decoded capture accepted")
			}
		})
	}
}

func TestRecordedShortCorpusRejectsBackwardsCapture(t *testing.T) {
	reference := make([][32]byte, 30)
	for i := range reference {
		reference[i] = sha256.Sum256([]byte{byte(i)})
	}
	if err := MatchRecording(reference, [][32]byte{reference[10], reference[11], reference[12], reference[9], reference[14], reference[15]}); err == nil {
		t.Fatal("backwards short-corpus capture accepted as wrap")
	}
	if err := MatchRecording(reference, [][32]byte{reference[27], reference[28], reference[29], reference[0], reference[1], reference[2]}); err != nil {
		t.Fatal(err)
	}
}
func TestRecordedDecoderDiagnosticCopyIsBounded(t *testing.T) {
	var buffer boundedDiagnostic
	if n, err := io.Copy(&buffer, bytes.NewBuffer(bytes.Repeat([]byte("x"), 128<<10))); err != nil || n != 128<<10 {
		t.Fatalf("copy: %d %v", n, err)
	}
	if len(buffer.String()) != 64<<10 {
		t.Fatal("io.Copy bypassed diagnostic bound")
	}
}
