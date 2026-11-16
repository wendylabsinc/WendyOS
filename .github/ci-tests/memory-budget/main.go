//go:build linux

// A bounded anonymous-memory workload for the real container memory-budget test.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "expected allocation size in MiB")
		os.Exit(2)
	}
	mib, err := strconv.Atoi(os.Args[1])
	if err != nil || mib < 1 || mib > 96 {
		fmt.Fprintln(os.Stderr, "allocation must be between 1 and 96 MiB")
		os.Exit(2)
	}
	data, err := syscall.Mmap(-1, 0, mib<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Fault in every page. Reserving virtual address space alone does not put
	// pressure on memory.max, and file-backed pages could simply be discarded.
	for offset := 0; offset < len(data); offset += os.Getpagesize() {
		data[offset] = byte(offset/os.Getpagesize()%251 + 1)
	}
	var checksum uint64
	for offset := 0; offset < len(data); offset += os.Getpagesize() {
		checksum += uint64(data[offset])
	}
	fmt.Printf("touched=%d checksum=%d\n", len(data), checksum)
	// Let the parent observe actual swapped pages while this allocation lives.
	time.Sleep(time.Second)
	runtime.KeepAlive(data)
}
