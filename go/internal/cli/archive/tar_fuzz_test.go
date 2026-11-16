package archive

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzTarExtraction(f *testing.F) {
	for mode := byte(0); mode < 6; mode++ {
		f.Add("file", []byte("contents"), mode)
	}
	f.Add("../../escape", []byte{}, byte(0))
	f.Fuzz(func(t *testing.T, name string, data []byte, mode byte) {
		if len(name) > 1024 || len(data) > 16<<10 {
			return
		}
		root := t.TempDir()
		dest, outside := filepath.Join(root, "dest"), filepath.Join(root, "outside")
		for _, dir := range []string{dest, outside} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{filepath.Join(dest, "old"), filepath.Join(outside, "sentinel")} {
			if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Exercise SafeJoin on the original arbitrary name without writing it.
		if joined, err := SafeJoin(dest, name); err == nil && joined != "" {
			rel, err := filepath.Rel(dest, joined)
			if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("escaped join: %q -> %q", name, joined)
			}
		}
		// Keep even a broken extractor's writes inside the test directory:
		// traversal and absolute entries point at its sibling sentinel.
		base := filepath.Base(name)
		if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
			base = "file"
		}
		entry := "files/" + base
		switch mode % 6 {
		case 1:
			entry = "../outside/sentinel"
		case 2:
			entry = filepath.Join(outside, "sentinel")
		case 3, 4:
			entry = "link/" + base
		}
		var buf bytes.Buffer
		writer := tar.NewWriter(&buf)
		write := func(name string, data []byte) error {
			if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			_, err := writer.Write(data)
			return err
		}
		if err := write("complete", []byte("new")); err != nil {
			t.Fatal(err)
		}
		if mode%6 == 3 || mode%6 == 4 {
			kind := byte(tar.TypeSymlink)
			if mode%6 == 4 {
				kind = tar.TypeLink
			}
			if err := writer.WriteHeader(&tar.Header{Name: "link", Typeflag: kind, Linkname: outside}); err != nil {
				t.Fatal(err)
			}
		}
		if mode%6 == 5 {
			if err := writer.WriteHeader(&tar.Header{Name: "partial", Mode: 0o644, Size: int64(len(data) + 1), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
			// Deliberately omit the last byte and tar footer.
		} else {
			if err := write(entry, data); err != nil {
				return
			} // Names tar itself cannot encode.
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		}
		err := ExtractTar(bytes.NewReader(buf.Bytes()), dest)
		if (mode%6 == 1 || mode%6 == 2 || mode%6 == 5) && err == nil {
			t.Fatal("accepted escaping or incomplete archive")
		}
		if err != nil {
			old, readErr := os.ReadFile(filepath.Join(dest, "old"))
			if readErr != nil || string(old) != "keep" {
				t.Fatal("failed extraction replaced destination")
			}
			entries, readErr := os.ReadDir(dest)
			if readErr != nil || len(entries) != 1 {
				t.Fatal("published partial extraction")
			}
		} else {
			got, readErr := os.ReadFile(filepath.Join(dest, entry))
			if readErr != nil || !bytes.Equal(got, data) {
				t.Fatalf("lost file contents: %v", readErr)
			}
			if _, err := os.Stat(filepath.Join(dest, "old")); !os.IsNotExist(err) {
				t.Fatal("old destination survived success")
			}
		}
		if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
			t.Fatal("temporary extraction leaked")
		}
		entries, readErr := os.ReadDir(outside)
		if readErr != nil || len(entries) != 1 {
			t.Fatal("wrote outside extraction")
		}
		sentinel, readErr := os.ReadFile(filepath.Join(outside, "sentinel"))
		if readErr != nil || string(sentinel) != "keep" {
			t.Fatal("overwrote outside sentinel")
		}
		if err := filepath.WalkDir(dest, func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type()&os.ModeSymlink != 0 {
				t.Fatal("extracted a symlink")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
