// Minimal TCP dial test to isolate Go networking issues.
// Usage: go run tcpdial.go HOST:PORT
package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tcpdial HOST:PORT")
		os.Exit(1)
	}
	addr := os.Args[1]

	// Test 1: Go net.Dial with "tcp" (dual-stack, non-blocking + kqueue)
	fmt.Printf("tcp  (Go net.Dial):   ")
	if conn, err := net.DialTimeout("tcp", addr, 5*time.Second); err != nil {
		fmt.Println("FAIL:", err)
	} else {
		conn.Close()
		fmt.Println("OK")
	}

	// Test 2: Go net.Dial with "tcp4" (IPv4 only, non-blocking + kqueue)
	fmt.Printf("tcp4 (Go net.Dial):   ")
	if conn, err := net.DialTimeout("tcp4", addr, 5*time.Second); err != nil {
		fmt.Println("FAIL:", err)
	} else {
		conn.Close()
		fmt.Println("OK")
	}

	// Test 3: Raw blocking syscall (like nc/Python)
	fmt.Printf("raw  (blocking):      ")
	host, port, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if ip == nil {
		fmt.Println("FAIL: invalid IP")
		return
	}
	p := 0
	fmt.Sscanf(port, "%d", &p)

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		fmt.Println("FAIL socket:", err)
		return
	}
	defer syscall.Close(fd)

	sa := &syscall.SockaddrInet4{Port: p}
	copy(sa.Addr[:], ip.To4())
	if err := syscall.Connect(fd, sa); err != nil {
		fmt.Println("FAIL connect:", err)
	} else {
		fmt.Println("OK")
	}
}
