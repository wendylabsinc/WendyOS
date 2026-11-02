package netwire

import (
	"bytes"
	"testing"
)

func TestHashTextByteRoundTrip(t *testing.T) {
	for _, want := range [][]byte{make([]byte, 32), bytes.Repeat([]byte{0xff}, 32),
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
			16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}} {
		text, err := HashText(want)
		if err != nil {
			t.Fatal(err)
		}
		if len(text) != 64 {
			t.Fatalf("hash text length = %d", len(text))
		}
		got, err := HashBytes(text)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("hash round trip = %x, want %x", got, want)
		}
	}
}
