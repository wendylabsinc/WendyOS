package roughtime

import (
	"crypto/sha512"
	"testing"
)

func TestVerifyMerklePathAndIndex(t *testing.T) {
	nonce := make([]byte, nonceSize)
	leaf := sha512.Sum512(append([]byte{0}, nonce...))
	sibling := sha512.Sum512([]byte{0, 1})
	left := sha512.Sum512(append(append([]byte{1}, leaf[:32]...), sibling[:32]...))
	right := sha512.Sum512(append(append([]byte{1}, sibling[:32]...), leaf[:32]...))
	for _, tt := range []struct {
		name  string
		root  []byte
		path  []byte
		index uint32
		valid bool
	}{
		{name: "single leaf", root: leaf[:32], valid: true},
		{name: "left leaf", root: left[:32], path: sibling[:32], valid: true},
		{name: "right leaf", root: right[:32], path: sibling[:32], index: 1, valid: true},
		{name: "truncated path", root: leaf[:32], path: sibling[:4]},
		{name: "trailing partial sibling", root: left[:32], path: append(sibling[:32:32], make([]byte, 4)...)},
		{name: "single leaf with index", root: leaf[:32], index: 1},
		{name: "unused index bits", root: right[:32], path: sibling[:32], index: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyMerkle(nonce, tt.root, tt.path, tt.index); got != tt.valid {
				t.Fatalf("verifyMerkle() = %v, want %v", got, tt.valid)
			}
		})
	}
}
