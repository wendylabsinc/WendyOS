package services

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mcuHelperPath owns the board's pins and flash layouts; only images that can flash a microcontroller ship it.
var mcuHelperPath = "/usr/libexec/wendyos/mcu-flash"

// mcuLayout is one flash map the helper knows, as `mcu-flash --layouts` prints it.
type mcuLayout struct {
	Name                   string
	LoaderStart, LoaderEnd uint64
	SketchStart, SketchEnd uint64
}

var (
	mcuLayoutsMu     sync.Mutex
	mcuLayoutsKnown  bool
	mcuLayoutsCached []mcuLayout
)

// loadMCULayouts caches the helper's answer, since the image cannot change
// under a running agent. A probe that failed to run is retried next time.
func loadMCULayouts() []mcuLayout {
	mcuLayoutsMu.Lock()
	defer mcuLayoutsMu.Unlock()
	if !mcuLayoutsKnown {
		mcuLayoutsCached, mcuLayoutsKnown = probeMCULayouts(mcuHelperPath)
	}
	return mcuLayoutsCached
}

// probeMCULayouts returns final=false when the helper could not be asked.
func probeMCULayouts(helper string) (layouts []mcuLayout, final bool) {
	if _, err := os.Stat(helper); err != nil {
		return nil, errors.Is(err, os.ErrNotExist)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, helper, "--layouts").Output()
	if err != nil {
		return nil, false
	}
	layouts, err = parseMCULayouts(string(out))
	if err != nil {
		return nil, true
	}
	return layouts, true
}

// parseMCULayouts reads lines like
// "zephyr-1 loader=0x08000000-0x080d0000 sketch=0x08100000-0x081c0000".
func parseMCULayouts(out string) ([]mcuLayout, error) {
	var layouts []mcuLayout
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("mcu layout line %q: want name loader=… sketch=…", line)
		}
		l := mcuLayout{Name: fields[0]}
		var err error
		if l.LoaderStart, l.LoaderEnd, err = parseMCURegion(fields[1], "loader="); err != nil {
			return nil, err
		}
		if l.SketchStart, l.SketchEnd, err = parseMCURegion(fields[2], "sketch="); err != nil {
			return nil, err
		}
		layouts = append(layouts, l)
	}
	return layouts, nil
}

func parseMCURegion(field, prefix string) (uint64, uint64, error) {
	start, end, ok := strings.Cut(strings.TrimPrefix(field, prefix), "-")
	if !strings.HasPrefix(field, prefix) || !ok {
		return 0, 0, fmt.Errorf("mcu layout field %q: want %s<start>-<end>", field, prefix)
	}
	s, err1 := strconv.ParseUint(start, 0, 64)
	e, err2 := strconv.ParseUint(end, 0, 64)
	if err1 != nil || err2 != nil || e <= s {
		return 0, 0, fmt.Errorf("mcu layout field %q: bad region", field)
	}
	return s, e, nil
}

// mcuFeatureEntries advertises each layout, so the CLI can refuse a core this
// image cannot flash before it compiles anything.
func mcuFeatureEntries(layouts []mcuLayout) []string {
	entries := make([]string, 0, len(layouts))
	for _, l := range layouts {
		entries = append(entries, "mcu-flash:"+l.Name)
	}
	return entries
}

func findMCULayout(layouts []mcuLayout, name string) (mcuLayout, bool) {
	for _, l := range layouts {
		if l.Name == name {
			return l, true
		}
	}
	return mcuLayout{}, false
}

// checkLoaderELF keeps the loader inside its region: OpenOCD writes every
// loadable segment, and the boot animation and storage sit right after it.
func checkLoaderELF(path string, l mcuLayout) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("loader is not a readable ELF: %w", err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		if p.Paddr < l.LoaderStart || p.Paddr+p.Filesz > l.LoaderEnd {
			return fmt.Errorf("loader segment 0x%08x+0x%x is outside the %s loader region 0x%08x-0x%08x",
				p.Paddr, p.Filesz, l.Name, l.LoaderStart, l.LoaderEnd)
		}
	}
	return nil
}
