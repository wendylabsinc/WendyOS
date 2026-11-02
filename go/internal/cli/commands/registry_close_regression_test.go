package commands

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func TestResolveRegistryForAgentRejectsClosedConnection(t *testing.T) {
	conn := &grpcclient.AgentConnection{RegistryDialer: func(context.Context, int) (net.Conn, error) {
		return nil, fmt.Errorf("unused")
	}}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := resolveRegistryForAgent(context.Background(), conn, 5555); err == nil {
		t.Fatal("registry proxy was returned for a closed connection")
	}
	dockerRegistryProxyCacheMu.Lock()
	_, retained := dockerRegistryProxyAddrs[conn]
	dockerRegistryProxyCacheMu.Unlock()
	if retained {
		t.Fatal("closed connection retained its registry proxy")
	}
}

func TestResolveRegistryForAgentConcurrentClosedResolution(t *testing.T) {
	for iteration := range 100 {
		conn := &grpcclient.AgentConnection{RegistryDialer: func(context.Context, int) (net.Conn, error) { return nil, fmt.Errorf("must not dial") }}
		_ = conn.Close()
		var workers sync.WaitGroup
		var successes atomic.Int32
		start := make(chan struct{})
		for range 32 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				_, _, _, err := resolveRegistryForAgent(context.Background(), conn, 5555)
				if err == nil {
					successes.Add(1)
				}
			}()
		}
		close(start)
		workers.Wait()
		dockerRegistryProxyCacheMu.Lock()
		_, retained := dockerRegistryProxyAddrs[conn]
		dockerRegistryProxyCacheMu.Unlock()
		if retained {
			t.Fatal("cache leaked after closed concurrent resolution")
		}
		if successes.Load() != 0 {
			t.Fatalf("iteration %d: %d resolves succeeded after connection Close completed", iteration, successes.Load())
		}
	}
}

func TestResolveRegistryForAgentConcurrentOpenResolutionAndClose(t *testing.T) {
	conn := &grpcclient.AgentConnection{RegistryDialer: func(context.Context, int) (net.Conn, error) { return nil, fmt.Errorf("must not dial") }}
	results := make(chan string, 32)
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			addr, _, _, err := resolveRegistryForAgent(context.Background(), conn, 5555)
			if err != nil {
				t.Error(err)
			}
			results <- addr
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var first string
	for addr := range results {
		if first == "" {
			first = addr
		}
		if addr != first {
			t.Fatalf("duplicate proxy: %q != %q", addr, first)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	dockerRegistryProxyCacheMu.Lock()
	_, retained := dockerRegistryProxyAddrs[conn]
	dockerRegistryProxyCacheMu.Unlock()
	if retained {
		t.Fatal("cache leaked after Close")
	}
	_, port, err := net.SplitHostPort(first)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err == nil {
		c.Close()
		t.Fatal("proxy listener survived Close")
	}
}

func TestResolveRegistryForAgentResolutionOverlapsClose(t *testing.T) {
	for range 100 {
		conn := &grpcclient.AgentConnection{RegistryDialer: func(context.Context, int) (net.Conn, error) { return nil, fmt.Errorf("must not dial") }}
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 16 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				_, _, _, _ = resolveRegistryForAgent(context.Background(), conn, 5555)
			}()
		}
		workers.Add(1)
		go func() { defer workers.Done(); <-start; _ = conn.Close() }()
		close(start)
		workers.Wait()
		_ = conn.Close()
		dockerRegistryProxyCacheMu.Lock()
		_, retained := dockerRegistryProxyAddrs[conn]
		dockerRegistryProxyCacheMu.Unlock()
		if retained {
			t.Fatal("cache retained proxy after all resolve/Close calls completed")
		}
	}
}
