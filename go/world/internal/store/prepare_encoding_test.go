package store

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// These digests were captured before the epoch-sort optimization. They bind
// both supported state encodings, including empty epoch arrays and policies.
func TestPreparationPreservesStateEncodings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		subscribed bool
		empty      bool
		want       string
	}{
		{"full", false, false, "be4640b806984757fad47e076a568b7e3d4553bc5ca6192bdfb388dafcf39f15"},
		{"full-empty-epochs", false, true, "f5b1b1ca7494a4deb08a175ba3173120b3d4fcfd703afb260bfb1609f1c5159e"},
		{"subscribed", true, false, "ff858cf5dd022140700bbdb85e462aa3c9f327e59e951deeb2bcb006629c3c8e"},
		{"subscribed-empty-epochs", true, true, "4f0d1c952d941c812ed24ee100acb1c25c742e9c4e8f2dcf0be1f14ca44b0407"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, epochs := autoresearchPreparationFixture(t)
			if tc.subscribed {
				s.subscription = storePolicy()
			}
			if tc.empty {
				clear(epochs)
			}
			prepared, err := s.prepare(r, epochs)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256(prepared.data))
			if got != tc.want {
				t.Fatalf("serialized state changed: got %s, want %s", got, tc.want)
			}
		})
	}
}
