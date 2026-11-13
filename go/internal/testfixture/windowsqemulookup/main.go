// windowsqemulookup records the real VM resolver before the hosted boot gate.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: windowsqemulookup EXPECTED_PACKAGED_EMULATOR")
		os.Exit(2)
	}
	expected := os.Args[1]
	record := map[string]any{
		"goos": runtime.GOOS, "goarch": runtime.GOARCH, "pointer_bits": strconv.IntSize,
		"program_files": os.Getenv("ProgramFiles"), "expected": expected,
	}
	info, statErr := os.Stat(expected)
	if statErr != nil {
		record["stat_error"] = statErr.Error()
	} else {
		record["mode"] = info.Mode().String()
		record["size"] = info.Size()
	}
	found, lookupErr := vm.FindQEMU(runtime.GOOS)
	record["found"] = found
	if lookupErr != nil {
		record["lookup_error"] = lookupErr.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
		fmt.Fprintln(os.Stderr, "write lookup evidence:", err)
		os.Exit(2)
	}
	if runtime.GOOS != "windows" || statErr != nil || !info.Mode().IsRegular() ||
		lookupErr != nil || !strings.EqualFold(filepath.Clean(found), filepath.Clean(expected)) {
		os.Exit(1)
	}
}
