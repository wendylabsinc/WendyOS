// Minimal TCP dial test to isolate Go networking issues.
// Usage: go run tcpdial.go HOST:PORT
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tcpdial HOST:PORT")
		os.Exit(1)
	}
	addr := os.Args[1]

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	conn.Close()
	fmt.Println("OK")
}
