package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStagedFilePathAcceptsRelativeRoot guards the regression where
// `wendy data download <episode> -o ./somedir` rejected every manifest file:
// filepath.Join cleans its result, so joining "./ep2.partial" with
// "events.jsonl" yields "ep2.partial/events.jsonl", which never carries the
// "./ep2.partial/" prefix the containment check compared against.
func TestStagedFilePathAcceptsRelativeRoot(t *testing.T) {
	abs := t.TempDir()
	cases := []struct {
		name string
		root string
		rel  string
	}{
		{"relative dot-slash root", "./ep2.partial", "events.jsonl"},
		{"relative dot-slash root nested file", "./ep2.partial", filepath.Join("camera", "0000.mp4")},
		{"bare relative root", "ep2.partial", "events.jsonl"},
		{"absolute root", abs, "events.jsonl"},
		{"absolute root nested file", abs, filepath.Join("camera", "0000.mp4")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stagedFilePath(tc.root, tc.rel)
			if err != nil {
				t.Fatalf("stagedFilePath(%q, %q) = error %v, want the file to resolve", tc.root, tc.rel, err)
			}
			wantRoot, err := filepath.Abs(tc.root)
			if err != nil {
				t.Fatalf("filepath.Abs(%q): %v", tc.root, err)
			}
			want := filepath.Join(wantRoot, tc.rel)
			if got != want {
				t.Fatalf("stagedFilePath(%q, %q) = %q, want %q", tc.root, tc.rel, got, want)
			}
		})
	}
}

// TestStagedFilePathRejectsUnsafePaths confirms the traversal protection still
// holds once the containment check is done on cleaned, absolute paths.
func TestStagedFilePathRejectsUnsafePaths(t *testing.T) {
	abs := t.TempDir()
	roots := []string{"./ep2.partial", "ep2.partial", abs}
	rels := []struct {
		name string
		rel  string
	}{
		{"empty", ""},
		{"parent", ".."},
		{"parent traversal", filepath.Join("..", "escape.jsonl")},
		{"nested traversal", filepath.Join("camera", "..", "..", "escape.jsonl")},
		{"unclean", "./events.jsonl"},
		{"trailing traversal", filepath.Join("camera", "..")},
		{"absolute", string(os.PathSeparator) + filepath.Join("etc", "passwd")},
	}
	for _, root := range roots {
		for _, tc := range rels {
			t.Run(root+"/"+tc.name, func(t *testing.T) {
				got, err := stagedFilePath(root, tc.rel)
				if err == nil {
					t.Fatalf("stagedFilePath(%q, %q) = %q, want an error", root, tc.rel, got)
				}
				base, absErr := filepath.Abs(root)
				if absErr != nil {
					t.Fatalf("filepath.Abs(%q): %v", root, absErr)
				}
				if got != "" && strings.HasPrefix(got, base+string(os.PathSeparator)) {
					t.Fatalf("stagedFilePath(%q, %q) returned %q inside the destination", root, tc.rel, got)
				}
			})
		}
	}
}
