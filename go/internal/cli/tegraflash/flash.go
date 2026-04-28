package tegraflash

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/tegraflash/bundle"
	"github.com/wendylabsinc/wendy/internal/cli/tegraflash/nv3p"
	"github.com/wendylabsinc/wendy/internal/cli/tegraflash/rcm"
)

const DefaultSkipLarger = int64(64 * 1024 * 1024)

// FlashOptions controls a Jetson USB recovery flash from a tegraflash bundle.
type FlashOptions struct {
	BundlePath string
	XMLName    string
	FullEMMC   bool
	SkipLarger int64
	Out        io.Writer
}

// Flash writes QSPI partitions, and optionally eMMC partitions, from a WendyOS
// tegraflash bundle to a Jetson in USB recovery mode.
func Flash(opts FlashOptions) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}

	fmt.Fprintln(out, "Opening tegraflash bundle...")
	b, err := bundle.Open(opts.BundlePath)
	if err != nil {
		return fmt.Errorf("opening bundle: %w", err)
	}
	defer b.Close()

	xmlData, xmlName, err := resolveLayoutXML(b, opts.XMLName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  Partition layout: %s\n", xmlName)

	layout, err := bundle.ParseLayout(xmlData)
	if err != nil {
		return fmt.Errorf("parsing partition XML: %w", err)
	}

	totalParts := countWritablePartitions(layout, opts.FullEMMC)
	fmt.Fprintf(out, "  Partitions to write: %d\n", totalParts)

	applet, err := b.Applet()
	if err != nil {
		return fmt.Errorf("extracting applet: %w", err)
	}
	fmt.Fprintf(out, "  applet_t234.bin: %d bytes\n", len(applet))

	bctData, bctName, err := firstBCT(b, layout)
	if err != nil {
		// T234 bundles do not include a pre-compiled BCT — it is generated from
		// DTS sources at flash time by tegrabct_v2. Skip DlBCT for these bundles.
		fmt.Fprintf(out, "  BCT: none (T234 bundle — will skip DlBCT)\n")
	} else {
		fmt.Fprintf(out, "  BCT: %s (%d bytes)\n", bctName, len(bctData))
	}

	fmt.Fprintln(out, "\nPut the Jetson into USB recovery mode:")
	fmt.Fprintln(out, "  1. Hold the REC / Force Recovery button")
	fmt.Fprintln(out, "  2. Press and release RESET")
	fmt.Fprintln(out, "  3. Release REC after about 2 seconds")
	fmt.Fprintln(out, "\nWaiting for device in recovery mode (up to 60 s)...")

	dev, err := rcm.WaitForDevice()
	if err != nil {
		return fmt.Errorf("waiting for device: %w", err)
	}
	fmt.Fprintf(out, "  Device: %s\n", dev.String())

	// UID is sent by the bootROM on enumeration; on macOS the IOKit layer drops it
	// before the interface is claimed. It is informational only for ODM-open devices.
	uid, err := dev.ReadUID()
	if err != nil {
		fmt.Fprintf(out, "  UID: (unavailable: %v)\n", err)
	} else {
		fmt.Fprintf(out, "  UID: %x\n", uid)
	}

	fmt.Fprintln(out, "Loading applet via RCM...")
	if err := dev.LoadApplet(applet); err != nil {
		dev.Close()
		return fmt.Errorf("loading applet: %w", err)
	}
	fmt.Fprintln(out, "  Applet sent; waiting for nv3p interface...")
	dev.Close()
	time.Sleep(500 * time.Millisecond)

	nv3pDev, err := rcm.WaitForNv3p()
	if err != nil {
		return fmt.Errorf("waiting for nv3p: %w", err)
	}
	defer nv3pDev.Close()
	fmt.Fprintln(out, "  nv3p interface ready")

	client, err := nv3p.NewClient(nv3pDev)
	if err != nil {
		return fmt.Errorf("opening nv3p session: %w", err)
	}

	info, err := client.GetPlatformInfo()
	if err != nil {
		return fmt.Errorf("getting platform info: %w", err)
	}
	fmt.Fprintf(out, "  Chip: 0x%04x  op_mode: 0x%x\n", info.ChipID.ID, info.OpMode)

	if len(bctData) > 0 {
		fmt.Fprintf(out, "Loading BCT (%d bytes)...\n", len(bctData))
		if err := client.DlBCT(bctData); err != nil {
			return fmt.Errorf("loading BCT: %w", err)
		}
	}

	written, skipped, err := writePartitions(out, client, b, layout, opts.FullEMMC, opts.SkipLarger)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nFlash complete: %d partitions written, %d skipped.\n", written, skipped)
	if skipped > 0 && !opts.FullEMMC {
		fmt.Fprintln(out, "  Some partitions skipped; use eMMC flashing to write onboard eMMC partitions.")
	}

	fmt.Fprintln(out, "Resetting device...")
	_ = client.Reset()
	return nil
}

func resolveLayoutXML(b *bundle.Bundle, xmlName string) ([]byte, string, error) {
	if xmlName != "" {
		xmlData, err := b.ExtractFile(xmlName)
		if err != nil {
			return nil, "", fmt.Errorf("extracting XML %s: %w", xmlName, err)
		}
		return xmlData, xmlName, nil
	}
	xmlData, found, err := b.FindXML()
	if err != nil {
		return nil, "", fmt.Errorf("finding partition XML: %w\n\nTip: use --tegraflash-xml <name> to specify the XML file", err)
	}
	return xmlData, found, nil
}

func countWritablePartitions(layout *bundle.PartitionLayout, fullEMMC bool) int {
	total := 0
	for i := range layout.Devices {
		dev := &layout.Devices[i]
		if !dev.IsQSPI() && (!fullEMMC || !dev.IsEMMC()) {
			continue
		}
		for j := range dev.Partitions {
			if dev.Partitions[j].HasFile() && !dev.Partitions[j].IsBCT() {
				total++
			}
		}
	}
	return total
}

func firstBCT(b *bundle.Bundle, layout *bundle.PartitionLayout) ([]byte, string, error) {
	for i := range layout.Devices {
		for j := range layout.Devices[i].Partitions {
			p := &layout.Devices[i].Partitions[j]
			if !p.IsBCT() || !p.HasFile() {
				continue
			}
			data, err := b.ExtractFile(p.Filename)
			if err != nil {
				return nil, "", fmt.Errorf("extracting BCT %s: %w", p.Filename, err)
			}
			return data, p.Filename, nil
		}
	}
	return nil, "", fmt.Errorf("BCT partition not found in XML; cannot initialise partition table")
}

func writePartitions(out io.Writer, client *nv3p.Client, b *bundle.Bundle, layout *bundle.PartitionLayout, fullEMMC bool, skipLarger int64) (int, int, error) {
	written := 0
	skipped := 0
	for i := range layout.Devices {
		devLayout := &layout.Devices[i]
		if devLayout.IsQSPI() {
			fmt.Fprintf(out, "\nWriting QSPI partitions (device %s instance %d):\n", devLayout.Type, devLayout.Instance)
		} else if devLayout.IsEMMC() && fullEMMC {
			fmt.Fprintf(out, "\nWriting eMMC partitions (device %s instance %d):\n", devLayout.Type, devLayout.Instance)
		} else {
			continue
		}

		for j := range devLayout.Partitions {
			p := &devLayout.Partitions[j]
			if !p.HasFile() || p.IsBCT() {
				continue
			}

			filename := strings.TrimSpace(p.Filename)
			partData, err := b.ExtractFile(filename)
			if err != nil {
				fmt.Fprintf(out, "  [SKIP] %s: file %q not in bundle (%v)\n", p.Name, filename, err)
				skipped++
				continue
			}

			if skipLarger > 0 && int64(len(partData)) > skipLarger {
				fmt.Fprintf(out, "  [SKIP] %s: %s is %d MB (exceeds limit %d MB)\n",
					p.Name, filename,
					int64(len(partData))/(1024*1024),
					skipLarger/(1024*1024))
				skipped++
				continue
			}

			fmt.Fprintf(out, "  Writing %-24s  %5d KB  (id=%d)... ", p.Name, len(partData)/1024, p.ID)
			if err := client.WritePartition(uint32(p.ID), 0x01, partData); err != nil {
				fmt.Fprintln(out, "FAILED")
				return written, skipped, fmt.Errorf("writing partition %s (id=%d): %w", p.Name, p.ID, err)
			}
			fmt.Fprintln(out, "OK")
			written++
		}
	}
	return written, skipped, nil
}
