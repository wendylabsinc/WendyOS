//go:build e2e_coverage && unix

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime/coverage"
	"time"
)

// Test-only local control, in the runner's private directory. It is never
// registered on the Agent's public server and absent from ordinary builds.
func init() {
	path := os.Getenv("WENDY_E2E_COVERAGE_SOCKET")
	if path == "" {
		return
	}
	listener, err := net.Listen("unix", path)
	if err == nil {
		err = os.Chmod(path, 0600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "E2E coverage control:", err)
		os.Exit(1)
	}
	// A re-executed helper must not bind the parent Agent's private socket.
	_ = os.Unsetenv("WENDY_E2E_COVERAGE_SOCKET")
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Serialize reset/export. The runner waits for each acknowledgement.
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			var request struct{ Operation, Directory string }
			err = json.NewDecoder(conn).Decode(&request)
			if err == nil {
				switch request.Operation {
				case "reset":
					err = coverage.ClearCounters()
				case "export":
					err = coverage.WriteMetaDir(request.Directory)
					if err == nil {
						err = coverage.WriteCountersDir(request.Directory)
					}
				default:
					err = fmt.Errorf("unknown coverage operation")
				}
			}
			message := ""
			if err != nil {
				message = err.Error()
			}
			_ = json.NewEncoder(conn).Encode(map[string]any{"error": message, "pid": os.Getpid()})
			_ = conn.Close()
		}
	}()
}
