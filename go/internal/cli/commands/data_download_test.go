package commands

import (
	"context"
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

// TestDownloadDestinationCleansTrailingSeparator guards the regression where
// `wendy data download <episode> -o ./ep/` staged INSIDE its own destination.
// The staging directory is named by appending ".partial" to the destination, so
// an uncleaned "./ep/" produced "./ep/.partial": os.MkdirAll then created the
// destination as a side effect, the final rename failed with ENOTEMPTY because
// the destination now held the staging directory, and every re-run refused with
// "destination already exists".
func TestDownloadDestinationCleansTrailingSeparator(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct {
		name      string
		id        string
		output    string
		wantDest  string
		wantStage string
	}{
		{"trailing separator", "ep1", "." + sep + "ep" + sep, filepath.Join(".", "ep"), filepath.Join(".", "ep") + ".partial"},
		{"bare relative", "ep1", "ep", "ep", "ep.partial"},
		{"dot slash", "ep1", "." + sep + "ep", filepath.Join(".", "ep"), filepath.Join(".", "ep") + ".partial"},
		{"default is the episode id", "ep1", "", "ep1", "ep1.partial"},
		{"redundant separators", "ep1", "out" + sep + sep + "ep" + sep, filepath.Join("out", "ep"), filepath.Join("out", "ep") + ".partial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest, stage := downloadDestination(tc.id, tc.output)
			if dest != tc.wantDest {
				t.Fatalf("downloadDestination(%q, %q) destination = %q, want %q", tc.id, tc.output, dest, tc.wantDest)
			}
			if stage != tc.wantStage {
				t.Fatalf("downloadDestination(%q, %q) stage = %q, want %q", tc.id, tc.output, stage, tc.wantStage)
			}
			// The staging directory must be a sibling of the destination, never
			// a child of it: a child is what made the rename fail.
			if strings.HasPrefix(stage, dest+sep) {
				t.Fatalf("stage %q is inside destination %q", stage, dest)
			}
		})
	}
}

// TestDownloadOneDiscardsOversizedStagedFile covers the resume wedge: resume
// starts from the staged file's own size, so a staged file longer than the
// manifest declares fails identically on every re-run until something discards
// it. The error must also name the staged path, which the user never chose and
// would otherwise have to guess.
func TestDownloadOneDiscardsOversizedStagedFile(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "events.jsonl")
	if err := os.WriteFile(staged, []byte("way too many bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	// The size check runs before the first RPC, so no client is needed.
	err := downloadOne(context.Background(), nil, "ep1", root, "events.jsonl", 4, "unused")
	if err == nil {
		t.Fatal("downloadOne accepted a staged file longer than the manifest")
	}
	if !strings.Contains(err.Error(), staged) {
		t.Fatalf("error does not name the staged path %q: %v", staged, err)
	}
	info, statErr := os.Stat(staged)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("staged file kept %d bytes; the next run would wedge the same way", info.Size())
	}
}

// TestDownloadOneDiscardsStagedFileWithWrongHash is the other wedge: a staged
// file that is already the full size asks the device for no bytes at all, so a
// re-run recomputes the same wrong hash forever.
func TestDownloadOneDiscardsStagedFileWithWrongHash(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "events.jsonl")
	contents := []byte("corrupt!")
	if err := os.WriteFile(staged, contents, 0o640); err != nil {
		t.Fatal(err)
	}

	client := &stubDataClient{}
	err := downloadOne(context.Background(), client, "ep1", root, "events.jsonl", int64(len(contents)), strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("downloadOne accepted a staged file whose hash does not match the manifest")
	}
	if !strings.Contains(err.Error(), staged) {
		t.Fatalf("error does not name the staged path %q: %v", staged, err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error does not name the cause: %v", err)
	}
	info, statErr := os.Stat(staged)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("staged file kept %d bytes; the next run would wedge the same way", info.Size())
	}
	if client.downloadOffset != int64(len(contents)) {
		t.Fatalf("resume offset = %d, want %d", client.downloadOffset, len(contents))
	}
}

// TestDownloadOneKeepsShortStagedFile is the counterpart: a staged file shorter
// than the manifest is a stream that ended early, its bytes are good, and the
// next run resumes from them. Discarding those would turn a slow uplink into an
// endless restart.
func TestDownloadOneKeepsShortStagedFile(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "events.jsonl")
	if err := os.WriteFile(staged, []byte("half"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := downloadOne(context.Background(), &stubDataClient{}, "ep1", root, "events.jsonl", 128, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("downloadOne reported success for a truncated download")
	}
	info, statErr := os.Stat(staged)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 4 {
		t.Fatalf("staged file size = %d, want the 4 resumable bytes kept", info.Size())
	}
}
