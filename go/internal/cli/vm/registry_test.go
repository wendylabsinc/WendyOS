package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestRegistryPortAllocatesAndReusesVerifiedForward(t *testing.T) {
	s := shortQMPStore(t)
	ln, err := net.Listen("unix", s.QMPPath("test"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		port, adds := 0, 0
		for range 2 {
			c, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			e, d := json.NewEncoder(c), json.NewDecoder(c)
			_ = e.Encode(map[string]any{"QMP": map[string]any{}})
			for {
				var req struct {
					Execute   string
					Arguments map[string]string
				}
				if d.Decode(&req) != nil {
					break
				}
				var result any = map[string]any{}
				if req.Execute == "human-monitor-command" {
					command := req.Arguments["command-line"]
					if command == "info usernet" {
						result = "Hub -1 (net0):\n"
						if port != 0 {
							result = fmt.Sprintf("Hub -1 (net0):\n TCP[HOST_FORWARD] 12 127.0.0.1 %d 10.0.2.15 5000 0 0\n", port)
						}
					} else {
						var candidate int
						if _, err := fmt.Sscanf(command, "hostfwd_add net0 tcp:127.0.0.1:%d-:5000", &candidate); err != nil {
							c.Close()
							done <- err
							return
						}
						adds++
						result = "Could not set up host forwarding rule"
						if adds > 1 {
							port = candidate
							result = ""
						}
					}
				}
				_ = e.Encode(map[string]any{"return": result})
			}
			c.Close()
		}
		if adds != 2 {
			done <- fmt.Errorf("added %d times, want one collision and one success", adds)
			return
		}
		done <- nil
	}()
	one, err := s.RegistryPort(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.RegistryPort(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if one == 0 || one != two {
		t.Fatalf("registry forward not stable: %d %d", one, two)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
