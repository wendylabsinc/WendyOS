package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func tarGz(t *testing.T, dir string, entries []entry) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tf, Linkname: e.linkname}
		if tf == tar.TypeDir {
			hdr.Size = 0
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bundle.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	// Shaped like a real flash bundle: a top-level directory holding the
	// descriptors and payloads.
	tarball := tarGz(t, dir, []entry{
		{name: "bundle/", typeflag: tar.TypeDir},
		{name: "bundle/rawprogram0.xml", body: "<data/>"},
		{name: "bundle/nested/deep/rootfs.img", body: "payload"},
	})
	dest := filepath.Join(dir, "out")
	if err := ExtractTarGz(tarball, dest); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "bundle", "rawprogram0.xml"))
	if err != nil || string(got) != "<data/>" {
		t.Errorf("descriptor = %q, %v", got, err)
	}
	// Intermediate directories must be created even when the archive omits them.
	if got, err := os.ReadFile(filepath.Join(dest, "bundle", "nested", "deep", "rootfs.img")); err != nil || string(got) != "payload" {
		t.Errorf("nested payload = %q, %v", got, err)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	// Bundles come from a remote manifest, so an entry must not escape dest.
	for _, name := range []string{"../escape", "bundle/../../escape", "/abs/escape"} {
		dir := t.TempDir()
		tarball := tarGz(t, dir, []entry{{name: name, body: "x"}})
		dest := filepath.Join(dir, "out")
		if err := ExtractTarGz(tarball, dest); err == nil {
			t.Errorf("%q: want an unsafe-path error, got nil", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "escape")); err == nil {
			t.Errorf("%q: wrote outside dest", name)
		}
	}
}

func TestExtractTarGzSkipsLinks(t *testing.T) {
	dir := t.TempDir()
	tarball := tarGz(t, dir, []entry{
		{name: "bundle/real", body: "ok"},
		{name: "bundle/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	dest := filepath.Join(dir, "out")
	if err := ExtractTarGz(tarball, dest); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "bundle", "link")); err == nil {
		t.Error("symlink was recreated; it should be skipped")
	}
	if _, err := os.Stat(filepath.Join(dest, "bundle", "real")); err != nil {
		t.Errorf("skipping a link must not abort the extraction: %v", err)
	}
}

func TestExtractTarGzLeavesNoPartialDestOnFailure(t *testing.T) {
	// A failed extraction must not leave a directory a later run would treat
	// as a complete bundle.
	dir := t.TempDir()
	tarball := tarGz(t, dir, []entry{{name: "bundle/ok", body: "x"}, {name: "../escape", body: "x"}})
	dest := filepath.Join(dir, "out")
	if err := ExtractTarGz(tarball, dest); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("dest exists after a failed extraction")
	}
	if _, err := os.Stat(dest + ".tmp"); err == nil {
		t.Error("temp dir was left behind")
	}
}

func TestExtractTarGzRejectsNonGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not.tar.gz")
	if err := os.WriteFile(path, []byte("this is not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(path, filepath.Join(dir, "out")); err == nil {
		t.Fatal("want an error for a non-gzip file")
	}
}

func TestExtractTarGzDropsGroupAndWorldWrite(t *testing.T) {
	// The archive is downloaded, so it must not be able to ask for a
	// group- or world-writable file. The exec bit has to survive: the Jetson
	// flashpack ships tools it runs.
	dir := t.TempDir()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range []struct {
		name string
		mode int64
	}{
		{"bundle/wide", 0o777},
		{"bundle/setuid", 0o4755},
		{"bundle/plain", 0o644},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(dir, "b.tar.gz")
	if err := os.WriteFile(tarball, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out")
	if err := ExtractTarGz(tarball, dest); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{
		"wide":   0o755,
		"setuid": 0o755,
		"plain":  0o644,
	} {
		fi, err := os.Stat(filepath.Join(dest, "bundle", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
		if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s kept a special bit: %v", name, fi.Mode())
		}
	}
}
