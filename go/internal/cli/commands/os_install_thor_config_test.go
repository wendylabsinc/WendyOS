package commands

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// newFATImage returns a path to a freshly-formatted bare FAT32 image labelled
// "config" — the shape make-thor-flashpack.sh ships as config-partition.fat32.img.
func newFATImage(t *testing.T) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), "config-partition.fat32.img")
	d, err := diskfs.Create(img, 33*1024*1024, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if _, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: "config"}); err != nil {
		t.Fatalf("format fat32: %v", err)
	}
	d.Close()
	return img
}

func readFATFile(t *testing.T, img, name string) ([]byte, error) {
	t.Helper()
	d, err := diskfs.Open(img)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer d.Close()
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatalf("get filesystem: %v", err)
	}
	return fs.ReadFile("/" + name)
}

// writeFAT wraps a fresh open of img in fatWriter and runs writeConfigFilesTo,
// mirroring injectConfigPartition minus the network agent download.
func writeFAT(t *testing.T, img string, agent []byte, creds []wendyconf.WifiCredential, name string, provJSON []byte) {
	t.Helper()
	d, err := diskfs.Open(img)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatalf("get filesystem: %v", err)
	}
	if err := writeConfigFilesTo(fatWriter{fs}, agent, creds, name, provJSON); err != nil {
		t.Fatalf("writeConfigFilesTo: %v", err)
	}
	d.Close()
}

func TestInjectConfigPartitionFAT(t *testing.T) {
	img := newFATImage(t)
	creds := []wendyconf.WifiCredential{{SSID: "Home", Password: "hunter2"}}
	writeFAT(t, img, []byte("BINARY"), creds, "brave-dolphin", []byte(`{"enrolled":true}`))

	conf, err := readFATFile(t, img, "wendy.conf")
	if err != nil {
		t.Fatalf("read wendy.conf: %v", err)
	}
	if got := string(conf); !strings.Contains(got, "ssid = Home") || !strings.Contains(got, "name = brave-dolphin") {
		t.Errorf("wendy.conf missing expected content:\n%s", got)
	}
	if b, err := readFATFile(t, img, "acme-enrollment.json"); err != nil || string(b) != `{"enrolled":true}` {
		t.Errorf("acme-enrollment.json = %q, %v", b, err)
	}
	if b, err := readFATFile(t, img, "wendy-agent"); err != nil || string(b) != "BINARY" {
		t.Errorf("wendy-agent = %q, %v", b, err)
	}
	// clock_floor is always written (8-byte payload).
	if b, err := readFATFile(t, img, "clock_floor"); err != nil || len(b) != 8 {
		t.Errorf("clock_floor len = %d, %v", len(b), err)
	}
}

// A re-run with different flags must not leave a stale tail from the prior write.
func TestInjectConfigPartitionRewriteIsClean(t *testing.T) {
	img := newFATImage(t)
	writeFAT(t, img, nil, nil, "a-longer-device-name", nil)
	writeFAT(t, img, nil, nil, "short-name", nil)

	b, err := readFATFile(t, img, "wendy.conf")
	if err != nil {
		t.Fatalf("read wendy.conf: %v", err)
	}
	if strings.Contains(string(b), "a-longer-device-name") {
		t.Errorf("stale content from prior write survived:\n%s", b)
	}
}

// closeFailFile accepts writes and fails the close, as a FAT driver can when
// the directory entry is flushed there.
type closeFailFile struct{ filesystem.File }

func (closeFailFile) Write(p []byte) (int, error) { return len(p), nil }
func (closeFailFile) Close() error                { return errors.New("flushing directory entry: no space left") }

type closeFailFS struct{ filesystem.FileSystem }

func (closeFailFS) OpenFile(string, int) (filesystem.File, error) { return closeFailFile{}, nil }

func TestFatWriterReportsCloseFailure(t *testing.T) {
	err := fatWriter{closeFailFS{}}.WriteFile("wendy.conf", []byte("[device]\nname = lab-nano\n"), 0o644)
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("WriteFile = %v; want the close failure", err)
	}
}
