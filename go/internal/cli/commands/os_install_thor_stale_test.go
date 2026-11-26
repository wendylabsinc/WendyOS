//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashengine"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// fakeThorGadget stands in for a Thor's flashing gadget: it keeps what is
// pushed, answers for the size of its disk, and records each write nvdd is
// asked to make.
type fakeThorGadget struct {
	diskSize int64
	sizeErr  error

	pushed map[string][]byte
	cmds   []string
	writes []fakeThorWrite
}

type fakeThorWrite struct {
	seq   int // index of the command in cmds
	start int64
	end   int64
	zeros bool
}

var fakeThorWriteFlags = regexp.MustCompile(`--inputbin=(\S+) .*--startoffset=(\d+)`)

func (g *fakeThorGadget) Push(r io.Reader, remotePath string, _ int) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if g.pushed == nil {
		g.pushed = map[string][]byte{}
	}
	g.pushed[remotePath] = data
	return nil
}

func (g *fakeThorGadget) Shell(cmd string) (string, error) {
	g.cmds = append(g.cmds, cmd)
	if strings.HasPrefix(cmd, "blockdev --getsize64 ") {
		if g.sizeErr != nil {
			return "", g.sizeErr
		}
		return strconv.FormatInt(g.diskSize, 10) + "\n", nil
	}
	if m := fakeThorWriteFlags.FindStringSubmatch(cmd); m != nil && strings.Contains(cmd, "/dev/nvme0n1") {
		data := g.pushed[m[1]]
		start, _ := strconv.ParseInt(m[2], 10, 64)
		g.writes = append(g.writes, fakeThorWrite{seq: len(g.cmds) - 1, start: start, end: start + int64(len(data)), zeros: isAll(data, 0)})
	}
	return "", nil
}

func (g *fakeThorGadget) qspiErase() int {
	return slices.IndexFunc(g.cmds, func(cmd string) bool { return strings.Contains(cmd, " -E eoffset=0,esize=0") })
}

// thorJobTestFlashpack is an extracted flash package and a job's copy of its
// flash-images, for a small layout shaped like the Thor's: QSPI firmware,
// then an NVMe whose GPT has a data partition that no row writes.
func thorJobTestFlashpack(t *testing.T) (*flashpack.Flashpack, string) {
	t.Helper()
	root := t.TempDir()
	img := buildTestGPTImage(t, 512, staleTestSectors, staleTestParts)
	backup := int64(len(img)) - 33*512
	files := map[string][]byte{
		"stage2/bundle/unified_flash/tools/flashtools/flash/nvdd": []byte("nvdd"),
		"run/bct.bin":           bytes.Repeat([]byte{0x5A}, 512),
		"run/mbr.bin":           img[:512],
		"run/gpt_primary.bin":   img[512 : 34*512],
		"run/app.img":           bytes.Repeat([]byte{0x5A}, 4096),
		"run/gpt_secondary.bin": img[backup:],
		"run/FileToFlash.txt": []byte(fmt.Sprintf(`/dev/block/810c5b0000.spi bct bct.bin 0 1048576 4 0 0 bct 4 - 0
/dev/nvme0n1 master_boot_record mbr.bin 0 512 1 0 0 master_boot_record 4 - 0
/dev/nvme0n1 primary_gpt gpt_primary.bin 512 16896 33 0 0 primary_gpt 4 - 0
/dev/nvme0n1 APP app.img %d %d 4096 0 0 APP 4 - 0
/dev/nvme0n1 secondary_gpt gpt_secondary.bin %d 16896 33 0 0 secondary_gpt 4 - 0
`, staleTestParts[0].first*512, (staleTestParts[0].last-staleTestParts[0].first+1)*512, backup)),
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &flashpack.Flashpack{Root: root}, filepath.Join(root, "run")
}

// TestThorInstallJobClearsThePreviousInstallation: an MCP install job writes
// a Thor through the same stage-2 engine as the terminal installer, so it
// must clear the data partition the flash does not write, before it erases
// QSPI or writes anything else.
func TestThorInstallJobClearsThePreviousInstallation(t *testing.T) {
	const disk = 64 << 20 // larger than the 20 MiB disk the layout declares
	fp, workspace := thorJobTestFlashpack(t)
	gadget := &fakeThorGadget{diskSize: disk}
	var phases []string
	checkpoint := func(phase string, beginsWrite bool) error {
		if len(gadget.cmds) != 0 {
			t.Errorf("the device ran %d commands before the job recorded %q", len(gadget.cmds), phase)
		}
		phases = append(phases, fmt.Sprintf("%s write=%v", phase, beginsWrite))
		return nil
	}
	if _, err := captureCommandStdout(t, func() error {
		return writeThorInstallJob(context.Background(), gadget, fp, workspace, func(string) {}, checkpoint)
	}); err != nil {
		t.Fatalf("writeThorInstallJob: %v", err)
	}
	if !slices.Equal(phases, []string{"writing_partitions write=true"}) {
		t.Errorf("checkpoints %v; want the write boundary once", phases)
	}

	var cleared []t234.Extent
	lastClear := -1
	for _, w := range gadget.writes {
		if w.zeros {
			cleared, lastClear = append(cleared, t234.Extent{Start: w.start, End: w.end}), w.seq
		}
	}
	diskEnd := int64(disk - 33*512)
	want := []t234.Extent{
		{Start: staleDataStart, End: staleDataStart + t234.StaleHeadBytes}, // where the old filesystem starts
		{Start: staleDataEnd - t234.StaleTailBytes, End: staleDataEnd},     // data's end as the layout sizes it
		{Start: diskEnd - t234.StaleTailBytes, End: diskEnd},               // data's end once first boot has grown it
	}
	if !slices.Equal(cleared, want) {
		t.Fatalf("cleared %v, want %v", cleared, want)
	}
	if erase := gadget.qspiErase(); erase < lastClear {
		t.Errorf("QSPI was erased at command %d, before the clearing ended at command %d", erase, lastClear)
	}
	for _, w := range gadget.writes {
		if !w.zeros && w.seq < lastClear {
			t.Errorf("[%d, %d) was written before the clearing ended", w.start, w.end)
		}
	}
}

// TestThorInstallJobStopsWhenOldDataCannotBeCleared: the job must fail, with
// the engine's reason, before QSPI is erased or a partition is written.
func TestThorInstallJobStopsWhenOldDataCannotBeCleared(t *testing.T) {
	fp, workspace := thorJobTestFlashpack(t)
	gadget := &fakeThorGadget{sizeErr: errors.New("exit status 1")}
	_, err := captureCommandStdout(t, func() error {
		return writeThorInstallJob(context.Background(), gadget, fp, workspace, func(string) {}, func(string, bool) error { return nil })
	})
	if !errors.Is(err, flashengine.ErrOldDataNotCleared) {
		t.Fatalf("writeThorInstallJob = %v, want ErrOldDataNotCleared", err)
	}
	if !strings.Contains(err.Error(), "/dev/nvme0n1") {
		t.Errorf("the job's error does not name the disk: %v", err)
	}
	if erase := gadget.qspiErase(); erase >= 0 || len(gadget.writes) != 0 {
		t.Errorf("QSPI erase at command %d and %d writes although the clearing failed", erase, len(gadget.writes))
	}
}

// The question that starts a Thor flash must say what the flash does: it now
// clears /data too, as the Orin flash does and says.
func TestThorEraseWarningNamesData(t *testing.T) {
	for _, want := range []string{"QSPI", "internal NVMe", "including /data", "cannot be undone"} {
		if !strings.Contains(thorEraseWarning, want) {
			t.Errorf("the Thor erase warning does not mention %q: %s", want, thorEraseWarning)
		}
	}
}
