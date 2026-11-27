package main

import "testing"

func TestDroppedAndReorderedChunksNeverProduceFrame(t *testing.T) {
	for _, bad := range []chunk{{Sequence: 1, Index: 2, Timestamp: 10, Flags: 2, Payload: []byte("c")}, {Sequence: 1, Index: 1, Timestamp: 11, Flags: 2, Payload: []byte("c")}} {
		var a assembler
		if a.add(chunk{Sequence: 1, Timestamp: 10, Payload: []byte("a")}) != nil {
			t.Fatal("early output")
		}
		if a.add(bad) != nil {
			t.Fatal("incomplete or mixed frame emitted")
		}
		f := a.add(chunk{Sequence: 2, Timestamp: 20, Flags: 3, Payload: []byte("fresh")})
		if f == nil || string(f.Payload) != "fresh" || f.Timestamp != 20 || f.Sequence != 2 {
			t.Fatal("did not recover with original metadata")
		}
	}
}

func TestOriginalMetadataWrapAndDuplicateFence(t *testing.T) {
	var a assembler
	a.add(chunk{Sequence: ^uint32(0), Timestamp: 10, Payload: []byte("a")})
	f := a.add(chunk{Sequence: ^uint32(0), Index: 1, Timestamp: 10, Flags: 2, Payload: []byte("b")})
	if f == nil || string(f.Payload) != "ab" || f.Sequence != ^uint32(0) || f.Timestamp != 10 {
		t.Fatal("metadata changed")
	}
	if a.add(chunk{Sequence: ^uint32(0), Timestamp: 11, Flags: 3, Payload: []byte("duplicate")}) != nil {
		t.Fatal("duplicate accepted")
	}
	if a.add(chunk{Sequence: 0, Timestamp: 20, Flags: 3, Payload: []byte("wrapped")}) == nil {
		t.Fatal("wrap rejected")
	}
}

func TestPhysicalAndRegularFilePathsRefused(t *testing.T) {
	for _, path := range []string{"/dev/cu.usbmodem2101", "/dev/ttyUSB0", "/dev/null", "/etc/hosts"} {
		if verifyPTY(path) == nil {
			t.Fatal("not a local PTY", path)
		}
	}
}
