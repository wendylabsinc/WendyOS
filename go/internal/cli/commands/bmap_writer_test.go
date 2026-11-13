package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The helper runs as root under sudo, where root's HOME holds no wendy config:
// the root command's init must not create one there.
func TestBmapWriteSkipsRootCommandInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows: os.UserHomeDir consults this
	t.Setenv("WENDY_CONFIG_DIR", "")
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"__bmap-write", "--device", filepath.Join(home, "missing"), "--bmap", "x.bmap"})
	_ = cmd.Execute()
	if _, err := os.Stat(filepath.Join(home, ".wendy")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("__bmap-write initialized wendy config in HOME (stat: %v)", err)
	}
}

func TestDownloadBmap(t *testing.T) {
	const body = `<bmap version="2.0"></bmap>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dst := t.TempDir() + "/x.bmap"
	if err := downloadBmap(srv.URL, dst); err != nil {
		t.Fatalf("downloadBmap: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestDownloadBmapHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := downloadBmap(srv.URL, t.TempDir()+"/x.bmap"); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestRunBmapWriteToFile(t *testing.T) {
	img, b := buildSparseImage() // from bmap_test.go (same package)
	dir := t.TempDir()
	bmapPath := dir + "/img.bmap"
	if err := os.WriteFile(bmapPath, []byte(bmapXML(b)), 0o644); err != nil {
		t.Fatalf("write bmap: %v", err)
	}
	devPath := dir + "/device.img"
	if err := os.WriteFile(devPath, make([]byte, b.ImageSize), 0o644); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := runBmapWrite(devPath, bmapPath, bytes.NewReader(img), 0); err != nil {
		t.Fatalf("runBmapWrite: %v", err)
	}
	got, _ := os.ReadFile(devPath)
	if !bytes.Equal(got, img) {
		t.Fatal("device content does not match image")
	}
}

func TestBmapWriteCommandIsHidden(t *testing.T) {
	root := NewRootCmd()
	c, _, err := root.Find([]string{"__bmap-write"})
	if err != nil {
		t.Fatalf("find __bmap-write: %v", err)
	}
	if c.Name() != "__bmap-write" || !c.Hidden {
		t.Fatalf("__bmap-write should be a hidden command; got name=%q hidden=%v", c.Name(), c.Hidden)
	}
}

// bmapXML renders a minimal valid bmap document for b (sha256, blocks in order).
func bmapXML(b *Bmap) string {
	var sb bytes.Buffer
	fmt.Fprintf(&sb, `<bmap version="2.0"><ImageSize>%d</ImageSize><BlockSize>%d</BlockSize>`,
		b.ImageSize, b.BlockSize)
	sb.WriteString(`<ChecksumType>sha256</ChecksumType><BlockMap>`)
	for _, r := range b.Ranges {
		fmt.Fprintf(&sb, `<Range chksum="%s">%d-%d</Range>`, r.Checksum, r.First, r.Last)
	}
	sb.WriteString(`</BlockMap></bmap>`)
	return sb.String()
}
